// Package proxy exposes the tunnel as local loopback proxies: an HTTP/1.1
// proxy with CONNECT (what browsers use for HTTPS) and a minimal SOCKS5
// proxy (what most other tools expect). Both dial through the tunnel.
package proxy

import (
        "bufio"
        "context"
        "encoding/binary"
        "fmt"
        "io"
        "net"
        "net/http"
        "time"
)

// DialFunc opens connections through the tunnel. network is "tcp".
// It is an alias so tunnel.DialFunc is directly usable here.
type DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

const dialTimeout = 15 * time.Second

// pipe shuttles bytes both ways; closing either side ends both copies.
func pipe(a, b net.Conn) {
        go func() {
                _, _ = io.Copy(b, a)
                _ = a.Close()
        }()
        _, _ = io.Copy(a, b)
        _ = a.Close()
        _ = b.Close()
}

// HTTPProxy is a loopback HTTP/1.1 proxy with CONNECT support.
type HTTPProxy struct {
        Dial DialFunc
}

// NewHTTPProxy returns a proxy dialing through d.
func NewHTTPProxy(d DialFunc) *HTTPProxy { return &HTTPProxy{Dial: d} }

// Serve accepts connections until the listener fails.
func (p *HTTPProxy) Serve(l net.Listener) error {
        for {
                c, err := l.Accept()
                if err != nil {
                        return err
                }
                go p.handle(c)
        }
}

func (p *HTTPProxy) handle(c net.Conn) {
        defer c.Close()
        br := bufio.NewReader(c)
        req, err := http.ReadRequest(br)
        if err != nil {
                return
        }
        if req.Method == http.MethodConnect {
                p.connect(c, req.Host)
                return
        }
        p.forward(c, req)
}

// connect serves a CONNECT request (all browser HTTPS goes through here).
func (p *HTTPProxy) connect(client net.Conn, host string) {
        ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
        defer cancel()
        up, err := p.Dial(ctx, "tcp", host)
        if err != nil {
                fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
                return
        }
        fmt.Fprintf(client, "HTTP/1.1 200 Connection established\r\n\r\n")
        pipe(client, up)
}

// forward serves plain absolute-URI HTTP requests (rare from modern
// browsers but expected of every HTTP proxy).
func (p *HTTPProxy) forward(client net.Conn, req *http.Request) {
        hc := &http.Client{
                Timeout: 60 * time.Second,
                Transport: &http.Transport{
                        DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                                return p.Dial(ctx, network, addr)
                        },
                },
        }
        req.RequestURI = "" // http.Client rejects inbound-style URIs
        resp, err := hc.Do(req)
        if err != nil {
                fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
                return
        }
        defer resp.Body.Close()
        _ = resp.Write(client)
}

// SOCKS5Proxy is a loopback SOCKS5 proxy: no auth, CONNECT command.
type SOCKS5Proxy struct {
        Dial DialFunc
}

// NewSOCKS5 returns a SOCKS5 proxy dialing through d.
func NewSOCKS5(d DialFunc) *SOCKS5Proxy { return &SOCKS5Proxy{Dial: d} }

// Serve accepts connections until the listener fails.
func (p *SOCKS5Proxy) Serve(l net.Listener) error {
        for {
                c, err := l.Accept()
                if err != nil {
                        return err
                }
                go p.handle(c)
        }
}

func (p *SOCKS5Proxy) handle(c net.Conn) {
        defer c.Close()
        br := bufio.NewReader(c)

        // Greeting: VER NMETHODS METHODS — accept "no authentication".
        hdr := make([]byte, 2)
        if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 || hdr[1] == 0 {
                return
        }
        methods := make([]byte, hdr[1])
        if _, err := io.ReadFull(br, methods); err != nil {
                return
        }
        if _, err := c.Write([]byte{5, 0}); err != nil {
                return
        }

        // Request: VER CMD RSV ATYP ADDR PORT — CONNECT only.
        head := make([]byte, 4)
        if _, err := io.ReadFull(br, head); err != nil || head[0] != 5 {
                return
        }
        if head[1] != 1 { // not CONNECT
                _, _ = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
                return
        }
        host, err := readAddr(br, head[3])
        if err != nil {
                _, _ = c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
                return
        }
        port := make([]byte, 2)
        if _, err := io.ReadFull(br, port); err != nil {
                return
        }
        addr := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(port)))

        ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
        defer cancel()
        up, err := p.Dial(ctx, "tcp", addr)
        if err != nil {
                _, _ = c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
                return
        }
        // Success reply: BND.ADDR/BND.PORT are irrelevant for the client here.
        _, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
        pipe(c, up)
}

// readAddr parses the ATYP/ADDR fields of a SOCKS5 request.
func readAddr(br *bufio.Reader, atyp byte) (string, error) {
        switch atyp {
        case 1: // IPv4
                b := make([]byte, 4)
                if _, err := io.ReadFull(br, b); err != nil {
                        return "", err
                }
                return net.IP(b).String(), nil
        case 3: // domain
                n := make([]byte, 1)
                if _, err := io.ReadFull(br, n); err != nil {
                        return "", err
                }
                d := make([]byte, n[0])
                if _, err := io.ReadFull(br, d); err != nil {
                        return "", err
                }
                return string(d), nil
        case 4: // IPv6
                b := make([]byte, 16)
                if _, err := io.ReadFull(br, b); err != nil {
                        return "", err
                }
                return net.IP(b).String(), nil
        default:
                return "", fmt.Errorf("unsupported ATYP %d", atyp)
        }
}
