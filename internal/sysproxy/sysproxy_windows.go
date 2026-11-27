//go:build windows

// Package sysproxy flips the per-user Windows system proxy (the setting
// Chrome and Edge follow) to Twilight's local HTTP proxy. It never touches
// machine-level settings, needs no admin rights, and restores whatever the
// user had before.
package sysproxy

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	// ProxyAddr is what the system proxy is pointed at (HTTP proxy port).
	ProxyAddr = "127.0.0.1:8080"
	keyPath   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

var (
	wininet              = syscall.NewLazyDLL("wininet.dll")
	procInternetSetOption = wininet.NewProc("InternetSetOptionW")
)

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

// Enable points the system proxy at Twilight and returns a restore
// function that puts the user's previous settings back.
func Enable() (restore func(), err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("open internet settings: %w", err)
	}
	defer k.Close()

	oldEnable, _, _ := k.GetIntegerValue("ProxyEnable")
	oldServer, _, _ := k.GetStringValue("ProxyServer")

	restore = func() {
		rk, e := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
		if e != nil {
			return
		}
		defer rk.Close()
		_ = rk.SetDWordValue("ProxyEnable", uint32(oldEnable))
		if oldServer != "" {
			_ = rk.SetStringValue("ProxyServer", oldServer)
		}
		notify()
	}

	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return nil, err
	}
	if err := k.SetStringValue("ProxyServer", ProxyAddr); err != nil {
		return nil, err
	}
	notify()
	return restore, nil
}

// Disable turns the system proxy off (used for self-heal after a crash).
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
	notify()
	return nil
}

// IsEnabled reports the current system-proxy state.
func IsEnabled() (enabled bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

// SelfHeal clears a Twilight proxy setting left behind by a crash — but
// only if it points at us; the user's own settings are never touched.
func SelfHeal() {
	if IsEnabled() {
		k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
		if err != nil {
			return
		}
		server, _, _ := k.GetStringValue("ProxyServer")
		k.Close()
		if server == ProxyAddr {
			_ = Disable()
		}
	}
}

// ErrUnsupported is the restore/no-op contract for non-Windows builds.
var ErrUnsupported = errors.New("system proxy toggle is Windows-only")

// notify tells WinINET clients (browsers) that the settings changed.
func notify() {
	procInternetSetOption.Call(0, internetOptionSettingsChanged, 0, 0)
	procInternetSetOption.Call(0, internetOptionRefresh, 0, 0)
}
