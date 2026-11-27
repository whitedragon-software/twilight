//go:build !windows

// Package sysproxy: on non-Windows builds the system-proxy toggle is a
// no-op; the local proxies remain usable when configured manually.
package sysproxy

import "errors"

// ProxyAddr mirrors the Windows value so callers can display it.
const ProxyAddr = "127.0.0.1:8080"

// ErrUnsupported explains why Enable fails on this OS.
var ErrUnsupported = errors.New("Windows-only; use 127.0.0.1:8080 manually")

// Enable always fails on non-Windows.
func Enable() (restore func(), err error) { return nil, ErrUnsupported }

// Disable is a no-op on non-Windows.
func Disable() error { return nil }

// IsEnabled always reports off on non-Windows.
func IsEnabled() (enabled bool) { return false }

// SelfHeal is a no-op on non-Windows.
func SelfHeal() {}
