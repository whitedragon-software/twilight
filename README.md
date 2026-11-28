# Twilight

A portable, single-binary Cloudflare WARP tunnel client written in Go — real device registration against the same client API the official Android app uses, a userspace WireGuard tunnel that needs no virtual NIC and no admin rights, loopback HTTP and SOCKS5 proxies, a per-user Windows system proxy toggle, DNS resolved inside the tunnel, and a minimal white control UI served from one embedded HTML page at `http://127.0.0.1:9090`. Nothing is installed, no service is created, and the official WARP client stays untouched: deleting the binary and its data folder is the complete uninstall.

---

## Contents

- `go.mod`, `go.sum` — module manifest (Go 1.26) and two pinned dependencies: `golang.zx2c4.com/wireguard` (the WireGuard device and the gVisor netstack) and `golang.org/x/sys` (Windows registry). Everything else is the standard library.
- `cmd/twilight/main.go` — verb dispatch (`serve`, `register`, `status`, `rotate`, `version`, `help`), the serve wiring (tunnel manager, loopback proxies, UI server, system-proxy hooks, graceful shutdown), console output.
- `internal/config/config.go` — the single on-disk artifact `twilight.json`: the Identity struct, data-directory resolution, load/save at 0600.
- `internal/registrar/registrar.go` — the Cloudflare WARP client API: registration, live device status, response normalization (port 0 → 2408, `/32` //`128`), error unwrapping.
- `internal/wireguard/keys.go` — X25519 keypair generation through `crypto/ecdh` with the WireGuard private-key clamping.
- `internal/tunnel/tunnel.go` — the userspace WireGuard session: connect/disconnect, the netstack dialer everything else uses, the liveness probe and `/cdn-cgi/trace`.
- `internal/proxy/proxy.go` — the loopback proxies: HTTP/1.1 with CONNECT and a minimal SOCKS5, both dialing through the tunnel.
- `internal/sysproxy/` — the per-user Windows system proxy toggle and crash self-heal (`sysproxy_windows.go`), and the no-op build for other platforms (`sysproxy_other.go`).
- `internal/webui/server.go`, `internal/webui/index.html` — the control UI: one embedded page and the small JSON API it polls.
- `CHANGELOG.md` — version history.

---

## How it works

### What "the app" is

`twilight` with no arguments (or `twilight serve`) is the whole product. It starts:

- the **tunnel manager** — idle until you press Connect;
- the **loopback proxies** — HTTP on `127.0.0.1:8080`, SOCKS5 on `127.0.0.1:1080`, listening from launch, dialing only while connected;
- the **UI server** — one embedded HTML page plus a JSON API on `127.0.0.1:9090`, bound to loopback only; and
- the **system-proxy hooks** — Windows-only, wired to Connect/Disconnect.

It opens the UI in your default browser (best-effort; `--no-browser` skips it, `--ui-addr` moves it) and prints the same URLs in the console. Ctrl+C — or closing the console window — disconnects the tunnel, restores the system proxy and shuts the server down, in that order on purpose, so a Ctrl+C is a clean disconnect, not a process kill.

The UI is a real UI that happens to live in a browser tab. That is deliberate: it keeps the GUI goal — a white, minimal, one-button surface — without shipping a browser runtime, an Electron layer or any second binary. The page is a hundred-odd lines of HTML/JS embedded in the executable with `go:embed`; the console window is the engine, and Ctrl+C is the off switch.

### What Connect actually does

Connect is two operations, and their failure modes are deliberately unequal:

1. **The tunnel comes up.** A wireguard-go `device.Device` is created over a gVisor netstack TUN (interface addresses and DNS taken from the identity), configured via `IpcSet` — hex-encoded keys on the wire (the file stores base64, WireGuard style), persistent keepalive 25 s, allowed IPs `0.0.0.0/0` + `::/0`, MTU 1420 — and brought up. Connect returns immediately; the WireGuard handshake completes asynchronously, and a background probe dialing `1.1.1.1:80` every 2 seconds (3-second timeout) flips the state from "Connecting — establishing handshake…" to "Connected" once traffic actually flows.
2. **The system proxy flips** (Windows). If that fails, Connect still succeeds and reports the reason in the UI — the tunnel and the manual proxies are never held hostage by the toggle.

Disconnect reverses both: tunnel torn down, then the exact previous proxy values written back.

### The loopback proxies

Both are thin pipes sharing one dial function — the tunnel's — behind them:

- **HTTP** (`127.0.0.1:8080`): `CONNECT` (what every browser does for HTTPS) and absolute-URI forwarding; a failed tunnel dial answers `502 Bad Gateway` instead of hanging.
- **SOCKS5** (`127.0.0.1:1080`): no-auth greeting, CONNECT command only, IPv4/domain/IPv6 address types, RFC 1928 failure replies.

They listen from launch, even before any registration, so an app pointed at them manually gets a clean "connection failed" while disconnected instead of a refused port. There is no authentication because both bind to loopback only — the boundary is the address, not a password.

### DNS

Hostnames are resolved by the netstack inside the tunnel (1.1.1.1 / 1.0.0.1 from the identity): a proxy client hands Twilight a domain, and the lookup happens over the tunnel's own resolver. The OS resolver is never asked, so proxy mode has no DNS leak.

### The system proxy toggle and self-heal

The Windows implementation touches exactly two values under `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings` — `ProxyEnable=1` and `ProxyServer=127.0.0.1:8080` — after capturing the previous ones into the restore closure, then calls `InternetSetOptionW` (settings-changed + refresh) so running browsers pick the change up without a restart. Per-user on purpose: no UAC, other Windows accounts unaffected, and nothing machine-wide that could collide with the official WARP client.

Because a crash mid-session could leave the proxy pointing at a dead `127.0.0.1:8080`, every launch runs self-heal: it clears the setting only when it points exactly at `127.0.0.1:8080` — Twilight's own signature — and never touches any other value the user had.

### Registration

Twilight registers a real WARP device against `api.cloudflareclient.com/v0a2158/reg` — the same programmatic interface used by `wgcf`-class tools — presenting an Android device profile with the `okhttp/3.12.1` user agent the real client sends. Both 200 and 201 are accepted; the response must carry an id, a token and exactly one peer. The stored identity is normalized on the way in:

- an endpoint port of `0` becomes `2408` — the API nominates anycast hosts and leaves the port to the client, which is what the official one does too;
- bare interface addresses get their WireGuard prefixes (`/32` for IPv4, `/128` for IPv6);
- DNS is pinned to 1.1.1.1 / 1.0.0.1.

`twilight status` and the UI's expiry line do a live `GET /reg/{id}` with the stored Bearer token — what they print reflects the server, not the local file. Error responses are unwrapped from the API's `errors[]` envelope and capped at 300 characters, so a failure message is a sentence, not a JSON dump. Registrations are observed to expire after roughly 90 days; the exact date is shown, and "New identity" (or `register --new`) replaces the device.

### Keys

One X25519 keypair per device, generated by the Go standard library (`crypto/ecdh`) — no `golang.org/x/crypto` dependency of Twilight's own. The private scalar is clamped the WireGuard way (`key[0] &= 248; key[31] &= 127; key[31] |= 64`) so the stored key is byte-identical in style to keys produced by the `wg` tools. The private key lives only in `twilight.json` and is never printed by any command or returned by any API endpoint.

### Portable state

Everything Twilight persists is one file: `twilight.json`, indented, hand-readable, written 0600, with a `version` field for future migrations. Where it lives is decided the same way on every verb:

1. `--data-dir DIR` — always wins (created if missing);
2. `twilight-data/` next to the executable — the portable mode, probed by actually creating and removing a file, so a read-only location silently falls through instead of failing at first write;
3. the per-user config directory — `%AppData%\Twilight` on Windows, `~/.config/Twilight` elsewhere.

Moving the binary and its data folder to a USB stick and another PC is the supported workflow: the identity keeps working, nothing is re-registered, and nothing was left behind on the old machine.

### Why the exit IP can't be "chosen"

WARP assigns egress addresses from Cloudflare's shared pool; there is no API to pick one. The honest equivalent — planned as M3 — is to pin the WireGuard endpoint to a specific colo (probing the candidate ports 2408/500/1701/4500 and verifying the colo via `/cdn-cgi/trace`, which this build already fetches) and to rotate on demand from the UI. Until then, `register --new` is the blunt rotation: a new device identity draws a new egress.

---

## Command line

| Command | Action |
|---|---|
| `twilight` or `twilight serve` | Run the app: UI at `http://127.0.0.1:9090`, HTTP proxy `127.0.0.1:8080`, SOCKS5 `127.0.0.1:1080`, browser auto-opened |
| `twilight serve --no-browser` | Same, without auto-opening the browser |
| `twilight serve --ui-addr ADDR` | Serve the UI on a different loopback address |
| `twilight register` | Register headless; refuses (exit 1) if an identity already exists |
| `twilight register --new` | Replace the identity with a fresh device (discards the old one) |
| `twilight status` | Print the local identity, then the live account state from Cloudflare |
| `twilight rotate` | Not yet implemented (M3); prints the plan and the `register --new` workaround |
| `twilight version` / `twilight help` | Version string / usage |
| `--data-dir DIR` (every verb) | Where `twilight.json` lives; default is exe-side `twilight-data/`, else the user config directory |

---

## Building

```
go build -trimpath -ldflags "-s -w" -o twilight ./cmd/twilight
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o twilight.exe ./cmd/twilight
```

The shipped binary is the Windows cross-compile; the source builds on any platform Go builds, with the system-proxy toggle compiling to a no-op off Windows. Dependencies are pinned in `go.mod` (`golang.zx2c4.com/wireguard` — the WireGuard device and the gVisor netstack — and `golang.org/x/sys` for the registry); the UI is embedded, so there is no asset pipeline, no Node and nothing to install besides Go.

---

## Known limitations

- Proxy mode, not a VPN: only apps that follow the system proxy (all major browsers) or that you point at the loopback proxies manually are tunneled. Raw sockets that ignore proxies bypass it — that is inherent to proxy mode. The system-wide TUN interface with a kill switch is M5.
- The system proxy toggle is Windows-only and per-user: other Windows accounts are unaffected, and on other platforms the toggle is a no-op (configure `127.0.0.1:8080` manually).
- The exit IP cannot be chosen; WARP draws from a shared pool. Colo pinning and on-demand rotation are M3; today `register --new` is the only rotation.
- SOCKS5 has no authentication and supports CONNECT only — no UDP ASSOCIATE. The HTTP proxy has no caching and speaks HTTP/1.1.
- Registrations expire after roughly 90 days (Cloudflare-side, observed). A stale identity simply stops handshaking; the UI and `status` show the date, and re-registering fixes it.
- The first launch triggers a SmartScreen warning (the binary is unsigned) — "More info" → "Run anyway".
- One identity per data directory: no profiles, no import/export, no multiple devices.

---

## Testing checklist

Work through these in order after any change to the tunnel, the proxies, the system-proxy toggle or the UI — each targets a specific layer, so a failure narrows down where to look.

| # | Action | What you're checking | Expected result |
|---|---|---|---|
| 1 | Launch `twilight.exe` from a standard (non-admin) account on a clean machine | First run | Console window opens, browser opens `http://127.0.0.1:9090`; UI shows "Not registered yet" with a **Register device** button; no UAC prompt ever appears |
| 2 | Click **Register device** | Registration | After a few seconds the button becomes **Connect**; `twilight-data\twilight.json` exists next to the exe |
| 3 | Run `twilight register` in a console | Duplicate guard | Exit code 1, "already registered", no second device created |
| 4 | Click **Connect** | Tunnel + handshake | Dot goes amber ("Connecting — establishing handshake…") then green within a few seconds; the exit line shows an IP, a colo code and `warp=on` |
| 5 | Open a "what is my IP" site in a normal browser tab | End-to-end traffic | Page loads; the IP it reports matches the UI's exit line |
| 6 | Check Settings → Network & internet → Proxy while connected | System proxy | Proxy On, `127.0.0.1:8080`; the UI meta line says "System proxy: on" |
| 7 | Note the current proxy settings, click **Disconnect**, check again | Restore | Dot gray; proxy off (or exactly whatever it was before); ordinary browsing still works |
| 8 | While connected, close the console window, then relaunch | Crash self-heal | The leftover proxy pointing at `127.0.0.1:8080` is cleared at launch ("System proxy: off"); a non-Twilight proxy setting would have been left alone |
| 9 | `curl --socks5-hostname 127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace` while connected, then while disconnected | SOCKS5 proxy | Connected: succeeds with `warp=on` in the trace. Disconnected: a SOCKS failure reply, not a hang |
| 10 | `curl -x http://127.0.0.1:8080 https://www.cloudflare.com/cdn-cgi/trace` while connected | HTTP proxy CONNECT | Succeeds through the tunnel |
| 11 | `twilight status` | Live status | Local identity block, then enabled / warp enabled / account type / expiry from the API; when offline it prints `warning: live status unavailable` and still exits 0 |
| 12 | `twilight register --new` | Identity replacement | New device id and keys in `twilight.json`; Connect still works afterwards |
| 13 | `twilight rotate` | Stub contract | Prints the M3 notice and the `register --new` workaround; changes nothing |
| 14 | Move exe + `twilight-data/` to a USB stick, run on another PC | Portability | Same identity, no re-registration, works immediately |
| 15 | Run the exe from a read-only directory | Data-dir fallback | `twilight.json` lands in `%AppData%\Twilight`; the UI and every verb agree on the location |
| 16 | `twilight serve --data-dir D:\elsewhere --no-browser` | Explicit data dir | State created exactly there; browser not auto-opened |
| 17 | `twilight serve --ui-addr 127.0.0.1:9999` | UI address flag | UI reachable on the chosen address; proxies unaffected |
| 18 | Start a second instance while one is running | Port conflict | Second instance exits with the UI-port error naming the conflict ("is another Twilight instance running?") |
| 19 | `twilight version` and `twilight help` | Console basics | Version string reads `twilight 0.1.0`; usage matches this README |
| 20 | Delete the folder (exe + `twilight-data`) | Uninstall | No registry value, no service, no file anywhere else; system proxy off |

---

## File map

```
cmd/twilight/main.go
├─ verb dispatch           → serve (default), register, status, rotate,
│                            version, help
├─ cmdServe()              → data-dir resolve, sysproxy.SelfHeal(), tunnel
│                            Manager, loopback proxies (8080 HTTP, 1080
│                            SOCKS), UI server (9090), browser open;
│                            Ctrl+C → restore system proxy → disconnect
│                            tunnel → server shutdown (2 s budget)
├─ cmdRegister()           → --new / --data-dir; refuses to overwrite an
│                            existing identity without --new (exit 1)
├─ cmdStatus()             → local identity + live Cloudflare query
└─ printIdentity()         → device id, public key, addresses, endpoint,
                             dns, account; private key is only ever named,
                             never printed

internal/config/config.go
├─ Resolve()               → --data-dir > exe-side twilight-data (write-
│                            probed) > user config dir (%AppData%\Twilight
│                            or ~/.config/Twilight)
├─ Load()/Save()           → twilight.json at 0600, indented, versioned
└─ Identity                → device id, API token, WireGuard keys, client
                             id, peer public key, endpoints, interface
                             addresses, DNS, account type/status

internal/registrar/registrar.go
├─ Register()              → POST /v0a2158/reg (Android profile, okhttp/
│                            3.12.1 UA; 200 and 201 accepted; one peer
│                            required; 20 s timeout, 1 MB body cap)
├─ DeviceStatus()          → GET /v0a2158/reg/{id}, Bearer token
├─ withDefaultPort()       → endpoint port "0" → 2408 (brackets for IPv6)
├─ withPrefix()            → bare addresses → /32 (v4), /128 (v6)
└─ apiErrorText()          → unwraps the errors[] envelope, caps at 300
                             characters

internal/wireguard/keys.go
└─ GenerateKeyPair()       → crypto/ecdh X25519 + WireGuard clamping
                             (key[0]&=248, key[31]&=127, key[31]|=64),
                             base64 out; PublicKeyFromPrivate for checks

internal/tunnel/tunnel.go
├─ Manager.Connect()       → netstack.CreateNetTUN (identity addresses,
│                            DNS 1.1.1.1), WireGuard device over
│                            conn.NewDefaultBind, IpcSet (hex keys,
│                            keepalive 25 s, 0.0.0.0/0 + ::/0), Up
├─ Manager.Disconnect()    → full teardown, probe goroutine stopped
├─ Manager.Dial()          → TCP dialer through the active tunnel;
│                            ErrNotConnected otherwise
├─ Manager.Trace()         → GET http://1.1.1.1/cdn-cgi/trace through the
│                            tunnel → ip / colo / warp / loc
└─ pollLoop()/probe()      → dials 1.1.1.1:80 every 2 s (3 s timeout) to
                             flag the handshake; ignores a racing
                             Disconnect

internal/proxy/proxy.go
├─ HTTPProxy               → CONNECT + absolute-URI forwarding; failed
│                            dial → 502; 15 s dial timeout
└─ SOCKS5Proxy             → no-auth greeting, CONNECT only, ATYP 1/3/4,
                             RFC 1928 failure replies

internal/sysproxy/
├─ sysproxy_windows.go     → HKCU Internet Settings: ProxyEnable/
│                            ProxyServer → 127.0.0.1:8080; old values
│                            captured for restore; InternetSetOptionW
│                            refresh; IsEnabled; SelfHeal clears only a
│                            proxy pointing at us
└─ sysproxy_other.go       → no-op build for non-Windows (manual
                             127.0.0.1:8080)

internal/webui/server.go + index.html
├─ index.html (embedded)   → the white single-page UI: state dot (gray /
│                            amber / green), main button (Register
│                            device / Connect / Disconnect), New
│                            identity, exit line, meta lines, 2 s poll
├─ GET  /                  → serves the embedded page
├─ GET  /api/status        → registered / active / handshake / uptime,
│                            system-proxy state and note, exit trace
│                            (cached 3 s), expiry (cached 1 min)
├─ POST /api/register      → {force}; 409 "already registered" without it
├─ POST /api/connect       → tunnel up, then system proxy on (tunnel
│                            survives a proxy-toggle failure)
├─ POST /api/disconnect    → tunnel down, system proxy restored
└─ GET  /api/trace         → on-demand /cdn-cgi/trace; 400 when the
                             tunnel is down
```
