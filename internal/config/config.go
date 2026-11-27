// Package config owns the single on-disk artifact: twilight.json.
// Portable-first: the file lives in a twilight-data folder next to the
// executable when that location is writable, otherwise in the user config
// directory. An explicit --data-dir always wins.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const FileName = "twilight.json"

// Identity is the entire on-disk state of Twilight: one flat file.
// WireGuard secrets are stored here; keep the file safe.
type Identity struct {
	Version       int      `json:"version"`
	RegisteredAt  string   `json:"registered_at"`
	DeviceType    string   `json:"device_type"`
	DeviceID      string   `json:"device_id"`
	APIToken      string   `json:"api_token"`
	PrivateKey    string   `json:"private_key"`
	PublicKey     string   `json:"public_key"`
	ClientID      string   `json:"client_id"`
	PeerPublicKey string   `json:"peer_public_key"`
	EndpointV4    string   `json:"endpoint_v4"`
	EndpointV6    string   `json:"endpoint_v6"`
	AddressV4     string   `json:"address_v4"`
	AddressV6     string   `json:"address_v6"`
	DNS           []string `json:"dns"`
	AccountType   string   `json:"account_type,omitempty"`
	AccountStatus string   `json:"account_status,omitempty"`
}

// ErrNoIdentity is returned when no twilight.json exists yet.
var ErrNoIdentity = errors.New("not registered: run `twilight register` first")

// Resolve returns the data directory to use.
// Priority: explicit flag > twilight-data next to the executable (portable
// mode, requires writable location) > user config directory.
func Resolve(flagDataDir string) (string, error) {
	if flagDataDir != "" {
		if err := os.MkdirAll(flagDataDir, 0o700); err != nil {
			return "", fmt.Errorf("create data dir: %w", err)
		}
		return flagDataDir, nil
	}
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "twilight-data")
		if dirWritable(candidate) {
			return candidate, nil
		}
	}
	fallback, err := userFallbackDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	return fallback, nil
}

func userFallbackDir() (string, error) {
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "Twilight"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	return filepath.Join(home, ".config", "Twilight"), nil
}

// dirWritable reports whether dir can be created and written to.
func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

// Path returns the full path of the identity file inside dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Exists reports whether the identity file exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Load reads and parses the identity file.
func Load(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", FileName, err)
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	return &id, nil
}

// Save writes the identity file with tight permissions (0600).
func Save(path string, id *Identity) error {
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	return nil
}
