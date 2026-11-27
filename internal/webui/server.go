// Package webui serves the embedded single-file UI plus the local JSON API
// the page talks to. Binds to loopback only; nothing here is exposed to
// the network.
package webui

import (
        _ "embed"
        "encoding/json"
        "net/http"
        "sync"
        "time"

        "twilight/internal/config"
        "twilight/internal/registrar"
        "twilight/internal/sysproxy"
        "twilight/internal/tunnel"
)

//go:embed index.html
var indexHTML []byte

// Ports the app listens on, loopback only.
const (
        UIPort    = "127.0.0.1:9090"
        HTTPPort  = "127.0.0.1:8080"
        SOCKSPort = "127.0.0.1:1080"
)

type cachedStatus struct {
        expires   string
        fetchedAt time.Time
}

// Server wires the tunnel manager, identity store and system-proxy state
// into HTTP handlers.
type Server struct {
        dataDir string
        mgr     *tunnel.Manager

        onConnect    func() (string, error) // returns (note, err); toggles system proxy
        onDisconnect func()

        mu          sync.Mutex
        trace       *tunnel.Trace
        traceAt     time.Time
        live        *cachedStatus
        connectNote string
}

// New builds the UI server. onConnect/onDisconnect let the host toggle the
// Windows system proxy around connect/disconnect.
func New(dataDir string, mgr *tunnel.Manager, onConnect func() (string, error), onDisconnect func()) *Server {
        return &Server{dataDir: dataDir, mgr: mgr, onConnect: onConnect, onDisconnect: onDisconnect}
}

// Handler returns the HTTP routing table.
func (s *Server) Handler() http.Handler {
        mux := http.NewServeMux()
        mux.HandleFunc("/", s.handleIndex)
        mux.HandleFunc("/api/status", s.handleStatus)
        mux.HandleFunc("/api/register", s.handleRegister)
        mux.HandleFunc("/api/connect", s.handleConnect)
        mux.HandleFunc("/api/disconnect", s.handleDisconnect)
        mux.HandleFunc("/api/trace", s.handleTrace)
        return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
                http.NotFound(w, r)
                return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        _, _ = w.Write(indexHTML)
}

type statusJSON struct {
        Registered bool         `json:"registered"`
        Active     bool         `json:"active"`
        Handshake  bool         `json:"handshake"`
        UptimeS    float64      `json:"uptime_s"`
        Error      string       `json:"error,omitempty"`
        Exit       *tunnel.Trace `json:"exit,omitempty"`
        SysProxy   struct {
                Enabled bool   `json:"enabled"`
                Note    string `json:"note,omitempty"`
                Addr    string `json:"addr"`
        } `json:"sysproxy"`
        Expires string `json:"expires,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
        out := statusJSON{}
        out.Registered = config.Exists(config.Path(s.dataDir))
        st := s.mgr.Status()
        out.Active = st.Active
        out.Handshake = st.Handshake
        out.UptimeS = st.UptimeS

        out.SysProxy.Enabled = sysproxy.IsEnabled()
        out.SysProxy.Addr = sysproxy.ProxyAddr
        s.mu.Lock()
        out.SysProxy.Note = s.connectNote
        s.mu.Unlock()

        if out.Registered {
                if exp := s.expiry(); exp != "" {
                        out.Expires = exp
                }
        }

        // Exit info: refresh at most every 3s while the tunnel carries traffic.
        if out.Active && out.Handshake {
                s.mu.Lock()
                fresh := s.trace != nil && time.Since(s.traceAt) < 3*time.Second
                var tr tunnel.Trace
                var err error
                if !fresh {
                        s.mu.Unlock()
                        tr, err = s.mgr.Trace()
                        s.mu.Lock()
                        if err == nil {
                                s.trace = &tr
                                s.traceAt = time.Now()
                        }
                }
                if s.trace != nil && (err == nil || fresh) {
                        out.Exit = s.trace
                }
                s.mu.Unlock()
        }
        writeJSON(w, out)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
        var body struct {
                Force bool `json:"force"`
        }
        _ = json.NewDecoder(r.Body).Decode(&body)
        path := config.Path(s.dataDir)
        if config.Exists(path) && !body.Force {
                writeErr(w, http.StatusConflict, "already registered; use 'New identity' to replace")
                return
        }
        id, err := registrar.Register()
        if err != nil {
                writeErr(w, http.StatusBadGateway, err.Error())
                return
        }
        if err := config.Save(path, id); err != nil {
                writeErr(w, http.StatusInternalServerError, err.Error())
                return
        }
        s.mu.Lock()
        s.trace = nil
        s.live = nil
        s.mu.Unlock()
        writeJSON(w, map[string]any{"ok": true, "device_id": id.DeviceID})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
        path := config.Path(s.dataDir)
        id, err := config.Load(path)
        if err != nil {
                writeErr(w, http.StatusBadRequest, err.Error())
                return
        }
        if err := s.mgr.Connect(id); err != nil {
                writeErr(w, http.StatusConflict, err.Error())
                return
        }
        if s.onConnect != nil {
                note, err := s.onConnect()
                s.mu.Lock()
                s.connectNote = note
                s.mu.Unlock()
                if err != nil {
                        // Tunnel stays up; system proxy just was not toggled.
                        writeJSON(w, map[string]any{"ok": true, "note": err.Error()})
                        return
                }
        }
        writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
        s.mgr.Disconnect()
        if s.onDisconnect != nil {
                s.onDisconnect()
        }
        s.mu.Lock()
        s.trace = nil
        s.mu.Unlock()
        writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
        if !s.mgr.Status().Active {
                writeErr(w, http.StatusBadRequest, "tunnel is not connected")
                return
        }
        tr, err := s.mgr.Trace()
        if err != nil {
                writeErr(w, http.StatusBadGateway, err.Error())
                return
        }
        writeJSON(w, tr)
}

// expiry returns the account TTL date, cached for a minute (one API call
// per minute instead of per UI poll).
func (s *Server) expiry() string {
        s.mu.Lock()
        defer s.mu.Unlock()
        if s.live != nil && time.Since(s.live.fetchedAt) < time.Minute {
                return s.live.expires
        }
        path := config.Path(s.dataDir)
        id, err := config.Load(path)
        if err != nil {
                return ""
        }
        expires := ""
        if st, err := registrar.DeviceStatus(id); err == nil {
                expires = splitDate(st.Account.TTL)
        }
        s.live = &cachedStatus{expires: expires, fetchedAt: time.Now()}
        return expires
}

func splitDate(ts string) string {
        for i := 0; i < len(ts); i++ {
                if ts[i] == 'T' {
                        return ts[:i]
                }
        }
        return ts
}

func writeJSON(w http.ResponseWriter, v any) {
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(code)
        _ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
