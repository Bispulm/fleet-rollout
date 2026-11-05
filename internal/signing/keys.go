package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

const (
	privateKeyPEMType = "PRIVATE KEY"
	publicKeyPEMType  = "PUBLIC KEY"
)

// GenerateKey creates a signing key.
func GenerateKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return key, nil
}

// MarshalPrivateKey encodes a key as PKCS#8 PEM, the format OpenSSL and others use.
func MarshalPrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: privateKeyPEMType, Bytes: der}), nil
}

// ParsePrivateKey decodes a PEM private key.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != privateKeyPEMType {
		return nil, errors.New("not a PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// The parser's error is dropped: it could describe key material.
		return nil, errors.New("malformed private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return key, nil
}

// MarshalPublicKey encodes a key as PKIX PEM.
func MarshalPublicKey(key ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: der}), nil
}

// ParseTrustSet reads one or more PEM public keys. More than one allows a key to be
// rotated by trusting the old and new together for a while. It fails if there are none,
// so a mistaken path can't produce a verifier that rejects everything by accident.
func ParseTrustSet(data []byte) (TrustSet, error) {
	var set TrustSet
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != publicKeyPEMType {
			return TrustSet{}, fmt.Errorf("unexpected PEM block %q in trust list", block.Type)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return TrustSet{}, fmt.Errorf("parse trusted key: %w", err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return TrustSet{}, errors.New("trusted keys must be Ed25519")
		}
		set.keys = append(set.keys, key)
	}
	if len(set.keys) == 0 {
		return TrustSet{}, errors.New("trust list has no public keys")
	}
	return set, nil
}
