# Twilight

Portable, minimal Cloudflare WARP tunnel client. One binary, one data file,
a clean white UI in your browser.

Status: **M2** — working tunnel. Run the app, press Connect, browse through
Cloudflare WARP. No admin rights, nothing installed, the official WARP
client is untouched.

## Quickstart (Windows)

1. Put `twilight.exe` in a folder (e.g. `C:\Users\you\Twilight`, or a USB stick).
2. Double-click it. A small console window opens (that's the engine) and
   your browser opens `http://127.0.0.1:9090`. First launch: SmartScreen
   asks once — "More info" → "Run anyway".
3. In the UI:
   - **Register device** — one click, talks to Cloudflare once, writes
     `twilight-data\twilight.json` next to the exe.
   - **Connect** — brings up the WireGuard tunnel (a green dot and your
     exit IP/colo appear when the handshake lands, ~2 s).
4. Open a new tab and visit Google — traffic now exits via WARP.

What Connect does under the hood: starts a userspace WireGuard tunnel
(no virtual NIC, no routes, no admin), starts `SOCKS5 127.0.0.1:1080` and
`HTTP 127.0.0.1:8080` loopback proxies, and flips the per-user Windows
system proxy to the HTTP proxy so Chrome/Edge route through the tunnel.
Disconnect restores your previous proxy settings. If the app ever dies
mid-session, the next launch self-heals (clears a leftover proxy pointing
at `127.0.0.1:8080` only).

To quit: close the console window or press Ctrl+C in it.
To uninstall: delete the folder. The machine stays untouched.

## Scope notes

- Traffic goes through the tunnel for apps that follow the system proxy
  (all browsers) or that you point at the SOCKS5/HTTP proxy manually.
  Raw sockets that ignore proxies bypass it — that is the nature of proxy
  mode; a system-wide TUN interface with kill switch arrives in M5.
- DNS names are resolved inside the tunnel (1.1.1.1) — no DNS leaks.
- Cloudflare device registrations expire after ~90 days (`status` shows
  the exact date; `register --new` / "New identity" refreshes it).
- Exit IP is Cloudflare's shared pool; M3 adds endpoint pin/rotate.

## CLI (still available)

```
twilight [serve] [--ui-addr ADDR] [--no-browser]   run the app (default)
twilight register [--new] [--data-dir DIR]         register headless
twilight status  [--data-dir DIR]                  identity + live account state
```

## Build

```
go build -trimpath -ldflags "-s -w" -o twilight ./cmd/twilight          # linux/mac
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o twilight.exe ./cmd/twilight
```

Dependencies: `golang.zx2c4.com/wireguard` (WireGuard device + gVisor
netstack) and `golang.org/x/sys` (Windows registry). Everything else is
the standard library; the UI is one embedded HTML file, no frameworks.
