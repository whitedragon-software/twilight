// Package registrar talks to the Cloudflare WARP client API — the same
// programmatic registration interface used by wgcf and similar open tools.
// M1 scope: register a device, query its status.
package registrar

import (
        "bytes"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "net/http"
        "strings"
        "time"

        "twilight/internal/config"
        "twilight/internal/wireguard"
)

const (
        apiBase    = "https://api.cloudflareclient.com"
        apiVersion = "v0a2158"
        userAgent  = "okhttp/3.12.1"
)

type registerBody struct {
        Key       string `json:"key"`
        InstallID string `json:"install_id"`
        FCMToken  string `json:"fcm_token"`
        Locale    string `json:"locale"`
        Model     string `json:"model"`
        Tos       string `json:"tos"`
        Type      string `json:"type"`
}

// apiResponse models only the fields Twilight needs; the API returns more.
type apiResponse struct {
        ID          string `json:"id"`
        Token       string `json:"token"`
        Enabled     bool   `json:"enabled"`
        WarpEnabled bool   `json:"warp_enabled"`
        Account     struct {
                Status      string `json:"status"`
                AccountType string `json:"account_type"`
                TTL         string `json:"ttl"`
        } `json:"account"`
        Config struct {
                ClientID string `json:"client_id"`
                Peers    []struct {
                        PublicKey string `json:"public_key"`
                        Endpoint  struct {
                                V4 string `json:"v4"`
                                V6 string `json:"v6"`
                        } `json:"endpoint"`
                } `json:"peers"`
                Interface struct {
                        Addresses struct {
                                V4 string `json:"v4"`
                                V6 string `json:"v6"`
                        } `json:"addresses"`
                } `json:"interface"`
        } `json:"config"`
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// Register creates a brand-new WARP device and returns the identity ready
// to be saved as twilight.json.
func Register() (*config.Identity, error) {
        keys, err := wireguard.GenerateKeyPair()
        if err != nil {
                return nil, err
        }
        body := registerBody{
                Key:    keys.Public,
                Locale: "en_US",
                Model:  "PC",
                Tos:    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
                Type:   "Android",
        }
        payload, err := json.Marshal(body)
        if err != nil {
                return nil, fmt.Errorf("encode request: %w", err)
        }

        req, err := http.NewRequest(http.MethodPost, apiBase+"/"+apiVersion+"/reg", bytes.NewReader(payload))
        if err != nil {
                return nil, err
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "application/json")
        req.Header.Set("User-Agent", userAgent)

        resp, err := httpClient.Do(req)
        if err != nil {
                return nil, fmt.Errorf("reach %s: %w", apiBase, err)
        }
        defer resp.Body.Close()
        raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if err != nil {
                return nil, fmt.Errorf("read response: %w", err)
        }
        if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
                return nil, fmt.Errorf("registration failed: HTTP %d: %s", resp.StatusCode, apiErrorText(raw))
        }

        var out apiResponse
        if err := json.Unmarshal(raw, &out); err != nil {
                return nil, fmt.Errorf("parse response: %w", err)
        }
        if out.ID == "" || out.Token == "" {
                return nil, fmt.Errorf("registration response missing id/token; the API layout may have changed")
        }
        if len(out.Config.Peers) != 1 {
                return nil, fmt.Errorf("expected exactly 1 peer in response, got %d", len(out.Config.Peers))
        }

        return &config.Identity{
                Version:       1,
                RegisteredAt:  time.Now().UTC().Format(time.RFC3339),
                DeviceType:    body.Type,
                DeviceID:      out.ID,
                APIToken:      out.Token,
                PrivateKey:    keys.Private,
                PublicKey:     keys.Public,
                ClientID:      out.Config.ClientID,
                PeerPublicKey: out.Config.Peers[0].PublicKey,
                EndpointV4:    withDefaultPort(out.Config.Peers[0].Endpoint.V4),
                EndpointV6:    withDefaultPort(out.Config.Peers[0].Endpoint.V6),
                AddressV4:     withPrefix(out.Config.Interface.Addresses.V4, false),
                AddressV6:     withPrefix(out.Config.Interface.Addresses.V6, true),
                DNS:           []string{"1.1.1.1", "1.0.0.1"},
                AccountType:   out.Account.AccountType,
                AccountStatus: out.Account.Status,
        }, nil
}

// DeviceStatus queries the live device/account state from the API.
func DeviceStatus(id *config.Identity) (*apiResponse, error) {
        req, err := http.NewRequest(http.MethodGet, apiBase+"/"+apiVersion+"/reg/"+id.DeviceID, nil)
        if err != nil {
                return nil, err
        }
        req.Header.Set("Accept", "application/json")
        req.Header.Set("User-Agent", userAgent)
        req.Header.Set("Authorization", "Bearer "+id.APIToken)

        resp, err := httpClient.Do(req)
        if err != nil {
                return nil, fmt.Errorf("reach %s: %w", apiBase, err)
        }
        defer resp.Body.Close()
        raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if err != nil {
                return nil, fmt.Errorf("read response: %w", err)
        }
        if resp.StatusCode != http.StatusOK {
                return nil, fmt.Errorf("status query failed: HTTP %d: %s", resp.StatusCode, apiErrorText(raw))
        }
        var out apiResponse
        if err := json.Unmarshal(raw, &out); err != nil {
                return nil, fmt.Errorf("parse response: %w", err)
        }
        return &out, nil
}

// withDefaultPort rewrites an API endpoint whose port is "0" to the
// standard WireGuard port 2408. The official client picks the port too;
// the API only nominates the host (anycast v4/v6).
func withDefaultPort(ep string) string {
        if ep == "" {
                return ep
        }
        host, port, err := net.SplitHostPort(ep)
        if err != nil || (port != "0" && port != "") {
                return ep
        }
        if strings.Contains(host, ":") { // IPv6 needs brackets
                return "[" + host + "]:2408"
        }
        return host + ":2408"
}

// withPrefix appends the WireGuard-style CIDR prefix when the API returns
// a bare interface address (v4: /32, v6: /128).
func withPrefix(addr string, v6 bool) string {
        if addr == "" || strings.Contains(addr, "/") {
                return addr
        }
        if v6 {
                return addr + "/128"
        }
        return addr + "/32"
}

// apiErrorText extracts a human-readable message from an API error body.
func apiErrorText(raw []byte) string {
        var wrapped struct {
                Errors []struct {
                        Code    int    `json:"code"`
                        Message string `json:"message"`
                } `json:"errors"`
        }
        if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Errors) > 0 {
                e := wrapped.Errors[0]
                return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
        }
        trimmed := bytes.TrimSpace(raw)
        if len(trimmed) == 0 {
                return "empty response body"
        }
        if len(trimmed) > 300 {
                trimmed = append(trimmed[:300], '.', '.', '.')
        }
        return string(trimmed)
}
