// Package auth handles the control plane's two separate credentials: a shared
// administrative token for operators, and a per-device token for devices.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	devicePrefix = "frd_"
	bearerPrefix = "Bearer "
	tokenBytes   = 32
)

// HashToken returns the value stored in place of a token. A token has 256 bits of
// entropy, so a fast hash is enough: there's nothing to guess by brute force.
func HashToken(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

// NewDeviceToken generates a device credential and the hash to store for it.
func NewDeviceToken() (string, [sha256.Size]byte, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("generate device token: %w", err)
	}
	token := devicePrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// AdminVerifier checks the administrative token.
type AdminVerifier struct {
	hash [sha256.Size]byte
}

// NewAdminVerifier returns a verifier for token.
func NewAdminVerifier(token string) AdminVerifier {
	return AdminVerifier{hash: HashToken(token)}
}

// Verify reports whether an Authorization header carries the admin token. It
// compares hashes in constant time, so timing reveals nothing about the token.
func (v AdminVerifier) Verify(header string) bool {
	token, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok {
		return false
	}
	hash := HashToken(token)
	return subtle.ConstantTimeCompare(hash[:], v.hash[:]) == 1
}
