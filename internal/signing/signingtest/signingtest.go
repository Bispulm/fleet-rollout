// Package signingtest creates keys and signed artifacts for tests.
package signingtest

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/signing"
)

// NewKey generates a signing key.
func NewKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	key, err := signing.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

// PublicPEM encodes a key's public half, the form a trust list file takes.
func PublicPEM(t testing.TB, key ed25519.PrivateKey) []byte {
	t.Helper()
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("not an Ed25519 key")
	}
	encoded, err := signing.MarshalPublicKey(public)
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}
	return encoded
}

// Trust returns a trust set holding the given keys.
func Trust(t testing.TB, keys ...ed25519.PrivateKey) signing.TrustSet {
	t.Helper()
	var encoded []byte
	for _, key := range keys {
		encoded = append(encoded, PublicPEM(t, key)...)
	}
	set, err := signing.ParseTrustSet(encoded)
	if err != nil {
		t.Fatalf("ParseTrustSet: %v", err)
	}
	return set
}

// Signed builds and signs a manifest for content.
func Signed(t testing.TB, key ed25519.PrivateKey, meta signing.Meta, content []byte) (signing.Manifest, signing.Envelope) {
	t.Helper()
	manifest, err := signing.BuildManifest(meta, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	envelope, err := signing.Sign(key, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return manifest, envelope
}
