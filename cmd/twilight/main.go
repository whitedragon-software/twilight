// Twilight — a portable, minimal Cloudflare WARP tunnel client.
//
// Run `twilight` (no arguments) or `twilight serve` to start the app: it
// serves the UI at http://127.0.0.1:9090, runs the WireGuard tunnel in
// userspace, exposes HTTP/SOCKS5 loopback proxies and flips the Windows
// system proxy on connect. `register` and `status` remain CLI verbs.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"twilight/internal/config"
	"twilight/internal/proxy"
	"twilight/internal/registrar"
	"twilight/internal/sysproxy"
	"twilight/internal/tunnel"
	"twilight/internal/webui"
)

const version = "0.2.0-m2"

func main() {
	args := os.Args[1:]
	verb := "serve"
	if len(args) > 0 {
		verb = args[0]
		args = args[1:]
	}
	switch verb {
	case "serve":
		cmdServe(args)
	case "register":
		cmdRegister(args)
	case "status":
		cmdStatus(args)
	case "rotate":
		fmt.Println("rotate: not implemented yet — arrives in M3 (endpoint pin/rotate).")
		fmt.Println("Interim workaround: `twilight register --new` replaces the identity (new egress).")
	case "version":
		fmt.Println("twilight " + version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", verb)
		usage()
		os.Exit(2)
	}
}

// cmdServe runs the full app: UI server + WireGuard tunnel manager +
// loopback proxies + system-proxy toggle, with graceful shutdown.
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "data directory for twilight.json")
	uiAddr := fs.String("ui-addr", webui.UIPort, "address for the UI server")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser automatically")
	fs.Parse(args)

	dir, err := config.Resolve(*dataDir)
	if err != nil {
		fatal("resolve data dir: %v", err)
	}

	// A previous crash may have left the system proxy pointing at us.
	sysproxy.SelfHeal()

	mgr := tunnel.NewManager()

	// System proxy toggle is wired to the UI's connect/disconnect hooks.
	sys := struct {
		on      bool
		restore func()
	}{}
	onConnect := func() (string, error) {
		restore, err := sysproxy.Enable()
		if err != nil {
			return err.Error(), err
		}
		sys.on = true
		sys.restore = restore
		return "", nil
	}
	onDisconnect := func() {
		if sys.on {
			sys.restore()
			sys.on = false
		}
	}
	defer onDisconnect()
	defer mgr.Disconnect()

	// Loopback proxies: always listening, dial only works while connected.
	httpProxy := proxy.NewHTTPProxy(mgr.Dial())
	socksProxy := proxy.NewSOCKS5(mgr.Dial())
	startProxy := func(addr string, p interface{ Serve(net.Listener) error }, label string) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			fatal("%s proxy on %s: %v", label, addr, err)
		}
		go func() { _ = p.Serve(l) }()
	}
	startProxy(webui.HTTPPort, httpProxy, "HTTP")
	startProxy(webui.SOCKSPort, socksProxy, "SOCKS5")

	ui := webui.New(dir, mgr, onConnect, onDisconnect)
	uiSrv := &http.Server{Addr: *uiAddr, Handler: ui.Handler()}
	uiLn, err := net.Listen("tcp", *uiAddr)
	if err != nil {
		fatal("UI server on %s: %v (is another Twilight instance running?)", *uiAddr, err)
	}
	go func() { _ = uiSrv.Serve(uiLn) }()

	url := "http://" + *uiAddr
	fmt.Printf("Twilight %s\n  UI         : %s\n  HTTP proxy : %s\n  SOCKS5     : %s\n  Data dir   : %s\n  Press Ctrl+C to quit.\n\n",
		version, url, webui.HTTPPort, webui.SOCKSPort, dir)
	if !*noBrowser {
		openBrowser(url)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	fmt.Println("\nshutting down ...")
	onDisconnect()
	mgr.Disconnect()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = uiSrv.Shutdown(shutdownCtx)
}

// openBrowser is best-effort; failure just means the user opens the URL.
func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("cmd", "/c", "start", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	default:
		_ = exec.Command("xdg-open", url).Start()
	}
}

func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "data directory for twilight.json")
	force := fs.Bool("new", false, "replace an existing identity with a fresh one")
	fs.Parse(args)

	path := identityPath(*dataDir)
	if config.Exists(path) && !*force {
		fmt.Printf("already registered: %s exists\n", path)
		fmt.Println("use `twilight register --new` to replace it (this discards the old identity)")
		os.Exit(1)
	}

	fmt.Println("registering with Cloudflare WARP ...")
	id, err := registrar.Register()
	if err != nil {
		fatal("register: %v", err)
	}
	if err := config.Save(path, id); err != nil {
		fatal("save: %v", err)
	}
	fmt.Printf("registered. identity written to %s\n\n", path)
	printIdentity(id)
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "data directory for twilight.json")
	fs.Parse(args)

	path := identityPath(*dataDir)
	id, err := config.Load(path)
	if err != nil {
		fatal("load: %v", err)
	}
	fmt.Println("local identity:")
	printIdentity(id)

	fmt.Println("\nquerying Cloudflare ...")
	st, err := registrar.DeviceStatus(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: live status unavailable: %v\n", err)
		os.Exit(0)
	}
	fmt.Println("live status:")
	fmt.Printf("  device enabled : %v\n", st.Enabled)
	fmt.Printf("  warp enabled   : %v\n", st.WarpEnabled)
	fmt.Printf("  account type   : %s\n", st.Account.AccountType)
	if st.Account.TTL != "" {
		fmt.Printf("  account expires: %s\n", strings.Split(st.Account.TTL, "T")[0])
	}
	fmt.Printf("  device type    : %s\n", id.DeviceType)
}

func printIdentity(id *config.Identity) {
	status := id.AccountStatus
	if status == "" {
		status = "unknown"
	}
	fmt.Printf("  device id     : %s\n", id.DeviceID)
	fmt.Printf("  public key    : %s\n", id.PublicKey)
	fmt.Printf("  addresses     : %s  %s\n", id.AddressV4, id.AddressV6)
	fmt.Printf("  endpoint      : %s\n", id.EndpointV4)
	fmt.Printf("  dns           : %s\n", strings.Join(id.DNS, ", "))
	fmt.Printf("  account       : %s (%s)\n", status, id.AccountType)
	fmt.Printf("  private key   : stored in %s — never printed, keep the file safe\n", config.FileName)
}

// identityPath resolves the data dir and returns the twilight.json path.
func identityPath(flagDataDir string) string {
	dir, err := config.Resolve(flagDataDir)
	if err != nil {
		fatal("resolve data dir: %v", err)
	}
	return config.Path(dir)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Print(`Twilight — portable Cloudflare WARP tunnel client (` + version + `)

Usage:
  twilight [serve] [--ui-addr ADDR] [--no-browser]   run the app: UI at http://127.0.0.1:9090
  twilight register [--new] [--data-dir DIR]         register with WARP; writes one file: twilight.json
  twilight status  [--data-dir DIR]                  show identity + live account status
  twilight rotate  [--data-dir DIR]                  (M3) endpoint pin/rotate — not yet implemented
  twilight version
  twilight help

The app: Connect/Disconnect in the browser UI, SOCKS5 proxy on 127.0.0.1:1080,
HTTP proxy on 127.0.0.1:8080. On Windows, Connect also flips the system proxy
(per-user, restored on disconnect). No admin rights anywhere.

Flags:
  --data-dir DIR   where twilight.json lives
                   (default: twilight-data/ next to the executable, else user config dir)

Portability: the binary needs no installation, no service, no admin rights.
Delete the binary and twilight.json and the machine is untouched.
`)
}
