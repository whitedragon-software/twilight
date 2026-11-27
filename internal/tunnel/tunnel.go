// Package tunnel drives a userspace WireGuard session to Cloudflare WARP.
//
// The entire tunnel — WireGuard device plus a TCP/IP stack (gVisor
// netstack) — runs in-process: no virtual NIC, no routes, no admin rights.
// Consumers dial TCP through the tunnel via the DialFunc handed out by
// Manager.Dial; DNS names are resolved inside the tunnel (1.1.1.1), so
// lookups never leak.
package tunnel

import (
        "context"
        "encoding/base64"
        "encoding/hex"
        "errors"
        "fmt"
        "net"
        "net/http"
        "net/netip"
        "strings"
        "sync"
        "time"

        "golang.zx2c4.com/wireguard/conn"
        "golang.zx2c4.com/wireguard/device"
        "golang.zx2c4.com/wireguard/tun"
        "golang.zx2c4.com/wireguard/tun/netstack"

        "twilight/internal/config"
)

const (
        mtu           = 1420
        probeAddr     = "1.1.1.1:80" // lightweight liveness check through the tunnel
        probeInterval = 2 * time.Second
        probeTimeout  = 3 * time.Second
)

// ErrNotConnected is returned by the dialer when no session is active.
var ErrNotConnected = errors.New("tunnel is not connected")

// DialFunc opens TCP connections through the active tunnel.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Status is the current session state.
type Status struct {
        Active    bool    `json:"active"`
        Handshake bool    `json:"handshake"`
        UptimeS   float64 `json:"uptime_s"`
}

// Trace is the Cloudflare edge view of the exit (/cdn-cgi/trace).
type Trace struct {
        IP   string `json:"ip"`
        Colo string `json:"colo"`
        Warp string `json:"warp"`
        Loc  string `json:"loc"`
}

// Manager owns at most one live session.
type Manager struct {
        mu        sync.Mutex
        dev       *device.Device
        tunDev    tun.Device
        tnet      *netstack.Net
        started   time.Time
        handshake bool
        stop      chan struct{}
}

// NewManager returns an idle manager.
func NewManager() *Manager { return &Manager{} }

// Connect brings up a WireGuard session for the identity. It returns as
// soon as the device is configured; the handshake completes asynchronously
// and is detected by the status poller.
func (m *Manager) Connect(id *config.Identity) error {
        m.mu.Lock()
        defer m.mu.Unlock()
        if m.dev != nil {
                return errors.New("tunnel already active")
        }

        localAddrs, err := tunnelAddresses(id)
        if err != nil {
                return err
        }
        dnsAddrs, err := parseAddrs(id.DNS, []netip.Addr{netip.MustParseAddr("1.1.1.1")})
        if err != nil {
                return err
        }

        tunDev, tnet, err := netstack.CreateNetTUN(localAddrs, dnsAddrs, mtu)
        if err != nil {
                return fmt.Errorf("create netstack: %w", err)
        }
        logger := device.NewLogger(device.LogLevelError, "twilight|")
        dev := device.NewDevice(tunDev, conn.NewDefaultBind(), logger)

        if err := applyConfig(dev, id); err != nil {
                dev.Close()
                return err
        }
        if err := dev.Up(); err != nil {
                dev.Close()
                return fmt.Errorf("bring up device: %w", err)
        }

        m.dev = dev
        m.tunDev = tunDev
        m.tnet = tnet
        m.started = time.Now()
        m.handshake = false
        stop := make(chan struct{})
        m.stop = stop
        go m.pollLoop(stop)
        return nil
}

// Disconnect tears down the session, if any.
func (m *Manager) Disconnect() {
        m.mu.Lock()
        dev := m.dev
        stop := m.stop
        m.dev = nil
        m.tnet = nil
        m.tunDev = nil
        m.handshake = false
        m.stop = nil
        m.mu.Unlock()
        if stop != nil {
                close(stop)
        }
        if dev != nil {
                dev.Close()
        }
}

// Status snapshots the session state.
func (m *Manager) Status() Status {
        m.mu.Lock()
        defer m.mu.Unlock()
        s := Status{Active: m.dev != nil, Handshake: m.handshake}
        if m.dev != nil {
                s.UptimeS = time.Since(m.started).Seconds()
        }
        return s
}

// Dial returns a dialer through the currently active tunnel.
func (m *Manager) Dial() DialFunc {
        return func(ctx context.Context, network, addr string) (net.Conn, error) {
                m.mu.Lock()
                tnet := m.tnet
                m.mu.Unlock()
                if tnet == nil {
                        return nil, ErrNotConnected
                }
                return tnet.DialContext(ctx, network, addr)
        }
}

// Trace fetches /cdn-cgi/trace from Cloudflare through the tunnel.
func (m *Manager) Trace() (Trace, error) {
        var tr Trace
        dial := m.Dial()
        ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
        defer cancel()
        client := &http.Client{
                Timeout: 8 * time.Second,
                Transport: &http.Transport{
                        DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
                                return dial(ctx, "tcp", addr)
                        },
                },
        }
        resp, err := client.Get("http://1.1.1.1/cdn-cgi/trace")
        if err != nil {
                return tr, err
        }
        defer resp.Body.Close()
        buf := make([]byte, 4096)
        n, _ := resp.Body.Read(buf)
        for _, line := range strings.Split(string(buf[:n]), "\n") {
                k, v, found := strings.Cut(strings.TrimSpace(line), "=")
                if !found {
                        continue
                }
                switch k {
                case "ip":
                        tr.IP = v
                case "colo":
                        tr.Colo = v
                case "warp":
                        tr.Warp = v
                case "loc":
                        tr.Loc = v
                }
        }
        return tr, nil
}

// pollLoop probes tunnel liveness until stop is closed.
func (m *Manager) pollLoop(stop chan struct{}) {
        ticker := time.NewTicker(probeInterval)
        defer ticker.Stop()
        m.probe()
        for {
                select {
                case <-stop:
                        return
                case <-ticker.C:
                        m.probe()
                }
        }
}

// probe dials a fixed in-tunnel address; success means the WireGuard
// handshake is complete and the session carries traffic.
func (m *Manager) probe() {
        m.mu.Lock()
        tnet := m.tnet
        active := m.dev != nil
        m.mu.Unlock()
        if tnet == nil || !active {
                return
        }
        ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
        c, err := tnet.DialContext(ctx, "tcp", probeAddr)
        cancel()
        if c != nil {
                c.Close()
        }
        m.mu.Lock()
        if m.dev != nil { // ignore probe racing a Disconnect
                m.handshake = err == nil
        }
        m.mu.Unlock()
}

// applyConfig pushes the identity into the WireGuard device. IpcSet uses
// hex-encoded keys; the identity file stores base64 (WireGuard style).
func applyConfig(dev *device.Device, id *config.Identity) error {
        priv, err := decodeKey(id.PrivateKey)
        if err != nil {
                return fmt.Errorf("private key: %w", err)
        }
        peer, err := decodeKey(id.PeerPublicKey)
        if err != nil {
                return fmt.Errorf("peer public key: %w", err)
        }
        cfg := fmt.Sprintf(
                "private_key=%s\nlisten_port=0\n"+
                        "public_key=%s\nendpoint=%s\npersistent_keepalive_interval=25\n"+
                        "allowed_ip=0.0.0.0/0\nallowed_ip=::/0\n",
                hex.EncodeToString(priv), hex.EncodeToString(peer), id.EndpointV4)
        return dev.IpcSet(cfg)
}

func decodeKey(b64 string) ([]byte, error) {
        raw, err := base64.StdEncoding.DecodeString(b64)
        if err != nil || len(raw) != 32 {
                return nil, fmt.Errorf("invalid base64 key (len %d)", len(raw))
        }
        return raw, nil
}

// tunnelAddresses parses the tunnel-local interface addresses.
func tunnelAddresses(id *config.Identity) ([]netip.Addr, error) {
        return parseAddrs([]string{id.AddressV4, id.AddressV6}, nil)
}

// parseAddrs parses address strings, falling back to def when none parse.
func parseAddrs(strs []string, def []netip.Addr) ([]netip.Addr, error) {
        var out []netip.Addr
        for _, s := range strs {
                if s == "" {
                        continue
                }
                if p, err := netip.ParsePrefix(s); err == nil {
                        out = append(out, p.Addr())
                        continue
                }
                a, err := netip.ParseAddr(s)
                if err != nil {
                        return nil, fmt.Errorf("address %q: %w", s, err)
                }
                out = append(out, a)
        }
        if len(out) == 0 {
                return def, nil
        }
        return out, nil
}
