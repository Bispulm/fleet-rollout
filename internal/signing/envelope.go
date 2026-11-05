package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	// PayloadType binds a signature to this use. A signature made for anything else
	// can't be replayed as a manifest, because the type is inside what's signed.
	PayloadType = "application/vnd.fleet-rollout.artifact-manifest.v1+json"

	// MaxEnvelopeBytes bounds what a verifier will parse. It comfortably fits a manifest
	// with the most chunks allowed.
	MaxEnvelopeBytes = 16 << 20

	// maxSignatures bounds the work one envelope can cause a verifier to do.
	maxSignatures = 8
)

// ErrInvalidSignature reports an envelope with no valid signature from a trusted key.
var ErrInvalidSignature = errors.New("no valid signature from a trusted key")

// Envelope is a DSSE envelope.
type Envelope struct {
	Payload     string      `json:"payload"`
	PayloadType string      `json:"payloadType"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one signature in an envelope. KeyID is an unauthenticated hint. As the
// spec requires, nothing here makes a decision based on it.
type Signature struct {
	KeyID string `json:"keyid,omitempty"`
	Sig   string `json:"sig"`
}

// PAE is DSSE's pre-authentication encoding, which is what actually gets signed.
func PAE(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("DSSEv1 ")
	b.WriteString(strconv.Itoa(len(payloadType)))
	b.WriteByte(' ')
	b.WriteString(payloadType)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(payload)))
	b.WriteByte(' ')
	b.Write(payload)
	return b.Bytes()
}

// KeyID derives the hint used in envelopes from a public key.
func KeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}

// Sign validates a manifest and wraps it in a signed envelope.
func Sign(key ed25519.PrivateKey, m Manifest) (Envelope, error) {
	if err := m.Validate(); err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode manifest: %w", err)
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return Envelope{}, errors.New("not an Ed25519 key")
	}
	return Envelope{
		Payload:     base64.StdEncoding.EncodeToString(payload),
		PayloadType: PayloadType,
		Signatures: []Signature{{
			KeyID: KeyID(public),
			Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(key, PAE(PayloadType, payload))),
		}},
	}, nil
}

// ParseEnvelope decodes an envelope without checking its signatures.
func ParseEnvelope(data []byte) (Envelope, error) {
	if len(data) > MaxEnvelopeBytes {
		return Envelope{}, fmt.Errorf("envelope is larger than %d bytes", MaxEnvelopeBytes)
	}
	var e Envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Envelope{}, errors.New("decode envelope: unexpected data after the document")
	}
	return e, nil
}

// decodeBase64 accepts standard and URL-safe encodings, as the spec allows.
func decodeBase64(s string) ([]byte, error) {
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return out, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// TrustSet is the public keys a verifier accepts signatures from.
type TrustSet struct {
	keys []ed25519.PublicKey
}

// Verify returns the manifest in an envelope, if a trusted key signed it and the manifest
// is valid. The payload is only decoded after its signature has been checked. The payload
// type is checked as well, since a signature proves who wrote something, not what it's for.
func (t TrustSet) Verify(e Envelope) (Manifest, error) {
	if len(e.Signatures) == 0 || len(e.Signatures) > maxSignatures {
		return Manifest{}, fmt.Errorf("%w: envelope has %d signatures", ErrInvalidSignature, len(e.Signatures))
	}
	payload, err := decodeBase64(e.Payload)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: payload isn't base64", ErrInvalidManifest)
	}
	if !t.anySignatureValid(e, payload) {
		return Manifest{}, ErrInvalidSignature
	}
	if e.PayloadType != PayloadType {
		return Manifest{}, fmt.Errorf("%w: unexpected payload type %q", ErrInvalidManifest, e.PayloadType)
	}

	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: unexpected data after the manifest", ErrInvalidManifest)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (t TrustSet) anySignatureValid(e Envelope, payload []byte) bool {
	message := PAE(e.PayloadType, payload)
	for _, signature := range e.Signatures {
		sig, err := decodeBase64(signature.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		for _, key := range t.keys {
			if ed25519.Verify(key, message, sig) {
				return true
			}
		}
	}
	return false
}
