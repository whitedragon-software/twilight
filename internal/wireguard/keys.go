// Package wireguard holds WireGuard primitives shared by all tunnel modes.
// M0/M1 scope: key generation only; device wiring arrives in M2.
package wireguard

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// KeyPair holds a WireGuard X25519 key pair, base64-encoded the way
// WireGuard configs expect.
type KeyPair struct {
	Private string // base64, never leaves the machine
	Public  string // base64, sent to Cloudflare at registration
}

// GenerateKeyPair creates a fresh X25519 key pair using the Go standard
// library (crypto/ecdh) — zero external dependencies.
// The private scalar is clamped per the WireGuard convention so the stored
// key is byte-identical in style to keys produced by wg tools.
func GenerateKeyPair() (KeyPair, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generate private key: %w", err)
	}
	raw := make([]byte, 32)
	copy(raw, priv.Bytes())
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64

	clamped, err := curve.NewPrivateKey(raw)
	if err != nil {
		return KeyPair{}, fmt.Errorf("clamp private key: %w", err)
	}
	return KeyPair{
		Private: base64.StdEncoding.EncodeToString(raw),
		Public:  base64.StdEncoding.EncodeToString(clamped.PublicKey().Bytes()),
	}, nil
}

// PublicKeyFromPrivate derives the base64 public key of a base64 private key.
func PublicKeyFromPrivate(privateB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(privateB64)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}
