package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

func trust(t *testing.T, keys ...ed25519.PrivateKey) TrustSet {
	t.Helper()
	var pemData []byte
	for _, key := range keys {
		encoded, err := MarshalPublicKey(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatalf("MarshalPublicKey: %v", err)
		}
		pemData = append(pemData, encoded...)
	}
	set, err := ParseTrustSet(pemData)
	if err != nil {
		t.Fatalf("ParseTrustSet: %v", err)
	}
	return set
}

func testMeta() Meta {
	return Meta{Name: "demo-app", Version: "1.4.0", Platform: "linux/arm64", ReleaseCounter: 14, ChunkSize: 4096}
}

func testManifest(t *testing.T) (Manifest, []byte) {
	t.Helper()
	content := bytes.Repeat([]byte("artifact-bytes!"), 1000) // 15,000 bytes.
	m, err := BuildManifest(testMeta(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	return m, content
}

// resign builds an envelope around an arbitrary payload, signed correctly, to prove that
// verification looks at the content and not just the signature.
func resign(t *testing.T, key ed25519.PrivateKey, payloadType string, payload []byte) Envelope {
	t.Helper()
	return Envelope{
		Payload:     base64.StdEncoding.EncodeToString(payload),
		PayloadType: payloadType,
		Signatures: []Signature{{
			Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, PAE(payloadType, payload))),
		}},
	}
}

func TestPAEMatchesTheSpecVector(t *testing.T) {
	got := PAE("http://example.com/HelloWorld", []byte("hello world"))
	const want = "DSSEv1 29 http://example.com/HelloWorld 11 hello world"
	if string(got) != want {
		t.Fatalf("PAE = %q, want %q", got, want)
	}
}

func TestPAECountsBytesNotCharacters(t *testing.T) {
	if got := string(PAE("é", []byte("ü"))); got != "DSSEv1 2 é 2 ü" {
		t.Fatalf("PAE = %q", got)
	}
}

func TestBuildManifestDescribesEveryChunk(t *testing.T) {
	m, content := testManifest(t)
	if m.SizeBytes != int64(len(content)) {
		t.Fatalf("size = %d", m.SizeBytes)
	}
	full := sha256.Sum256(content)
	if m.SHA256 != hex.EncodeToString(full[:]) {
		t.Fatal("full digest doesn't match the content")
	}
	if len(m.Chunks) != 4 { // 4096 * 3 = 12,288, so a short fourth chunk.
		t.Fatalf("chunks = %d, want 4", len(m.Chunks))
	}
	for i, digest := range m.Chunks {
		start := int64(i) * m.ChunkSize
		end := min(start+m.ChunkSize, m.SizeBytes)
		sum := sha256.Sum256(content[start:end])
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("chunk %d digest doesn't match", i)
		}
		if m.ChunkOffset(i) != start || m.ChunkLength(i) != end-start {
			t.Fatalf("chunk %d = offset %d length %d", i, m.ChunkOffset(i), m.ChunkLength(i))
		}
	}
}

func TestBuildManifestHandlesAnExactMultipleOfTheChunkSize(t *testing.T) {
	meta := testMeta()
	m, err := BuildManifest(meta, bytes.NewReader(make([]byte, 2*meta.ChunkSize)))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(m.Chunks))
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestBuildManifestRejectsEmptyContent(t *testing.T) {
	if _, err := BuildManifest(testMeta(), bytes.NewReader(nil)); err == nil {
		t.Fatal("built a manifest for an empty file")
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	envelope, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := trust(t, key).Verify(envelope)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.SHA256 != m.SHA256 || got.ReleaseCounter != m.ReleaseCounter || len(got.Chunks) != len(m.Chunks) {
		t.Fatalf("verified manifest = %+v", got)
	}
	if envelope.PayloadType != PayloadType {
		t.Fatalf("payload type = %q", envelope.PayloadType)
	}
}

func TestEnvelopeSurvivesJSONEncoding(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	envelope, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseEnvelope(encoded)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if _, err := trust(t, key).Verify(parsed); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyAcceptsURLSafeBase64AsTheSpecAllows(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	envelope, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	envelope.Payload = base64.URLEncoding.EncodeToString(raw)
	if _, err := trust(t, key).Verify(envelope); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsForgeries(t *testing.T) {
	key, other := testKey(t), testKey(t)
	m, _ := testManifest(t)
	good, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(good.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	tampered := bytes.Replace(payload, []byte(`"release_counter":14`), []byte(`"release_counter":99`), 1)
	if bytes.Equal(tampered, payload) {
		t.Fatal("the test's substitution didn't apply")
	}

	cases := map[string]Envelope{
		"no signatures":        {Payload: good.Payload, PayloadType: good.PayloadType},
		"empty signature list": {Payload: good.Payload, PayloadType: good.PayloadType, Signatures: []Signature{}},
		"untrusted key":        resign(t, other, PayloadType, payload),
		"tampered payload": {
			Payload:     base64.StdEncoding.EncodeToString(tampered),
			PayloadType: good.PayloadType,
			Signatures:  good.Signatures,
		},
		// A real signature over the same bytes, but for some other purpose.
		"signature for another type": func() Envelope {
			e := resign(t, key, "application/vnd.something-else+json", payload)
			e.PayloadType = PayloadType
			return e
		}(),
		"signature over the raw payload, without PAE": {
			Payload:     good.Payload,
			PayloadType: good.PayloadType,
			Signatures:  []Signature{{Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))}},
		},
		"trusted key's ID on another key's signature": func() Envelope {
			e := resign(t, other, PayloadType, payload)
			e.Signatures[0].KeyID = KeyID(key.Public().(ed25519.PublicKey))
			return e
		}(),
		"signature that isn't base64": {
			Payload:     good.Payload,
			PayloadType: good.PayloadType,
			Signatures:  []Signature{{Sig: "!!!"}},
		},
	}
	set := trust(t, key)
	for name, envelope := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := set.Verify(envelope); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("error = %v, want ErrInvalidSignature", err)
			}
		})
	}
}

func TestVerifyRejectsTheWrongPayloadTypeEvenWhenSigned(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	good, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(good.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	envelope := resign(t, key, "application/vnd.something-else+json", payload)
	if _, err := trust(t, key).Verify(envelope); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("error = %v, want ErrInvalidManifest", err)
	}
}

func TestVerifyRejectsMalformedPayload(t *testing.T) {
	key := testKey(t)
	set := trust(t, key)
	cases := map[string]string{
		"not base64":    "",
		"not JSON":      "not json",
		"unknown field": `{"schema":"x","surprise":true}`,
		"trailing data": `{} {}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			envelope := resign(t, key, PayloadType, []byte(payload))
			if name == "not base64" {
				envelope.Payload = "!!!"
			}
			_, err := set.Verify(envelope)
			if !errors.Is(err, ErrInvalidManifest) && !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// A signature proves who wrote a manifest, not that it makes sense. A correctly signed
// manifest that lies about its own structure must still be refused.
func TestVerifyRejectsSignedManifestsThatDontAddUp(t *testing.T) {
	key := testKey(t)
	set := trust(t, key)
	cases := map[string]func(*Manifest){
		"bad schema":             func(m *Manifest) { m.Schema = "other" },
		"bad name":               func(m *Manifest) { m.Name = "Not Valid" },
		"bad version":            func(m *Manifest) { m.Version = "1 0" },
		"bad platform":           func(m *Manifest) { m.Platform = "linux" },
		"zero release":           func(m *Manifest) { m.ReleaseCounter = 0 },
		"zero size":              func(m *Manifest) { m.SizeBytes = 0 },
		"negative size":          func(m *Manifest) { m.SizeBytes = -1 },
		"short digest":           func(m *Manifest) { m.SHA256 = "abc" },
		"uppercase digest":       func(m *Manifest) { m.SHA256 = strings.ToUpper(m.SHA256) },
		"zero chunk size":        func(m *Manifest) { m.ChunkSize = 0 },
		"chunk size too small":   func(m *Manifest) { m.ChunkSize = 1 },
		"chunk size too large":   func(m *Manifest) { m.ChunkSize = 1 << 40 },
		"too few chunks":         func(m *Manifest) { m.Chunks = m.Chunks[:1] },
		"too many chunks":        func(m *Manifest) { m.Chunks = append(m.Chunks, m.Chunks[0]) },
		"no chunks":              func(m *Manifest) { m.Chunks = nil },
		"malformed chunk digest": func(m *Manifest) { m.Chunks[0] = "nope" },
		"size beyond the chunks": func(m *Manifest) { m.SizeBytes = int64(len(m.Chunks))*m.ChunkSize + 1 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := testManifest(t)
			edit(&m)
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if _, err := set.Verify(resign(t, key, PayloadType, encoded)); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("error = %v, want ErrInvalidManifest", err)
			}
		})
	}
}

func TestVerifyAcceptsAnySignatureFromATrustedKey(t *testing.T) {
	key, stranger := testKey(t), testKey(t)
	m, _ := testManifest(t)
	mine, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	theirs, err := Sign(stranger, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	mine.Signatures = append(theirs.Signatures, mine.Signatures...)
	if _, err := trust(t, key).Verify(mine); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyBoundsTheNumberOfSignatures(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	envelope, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	for len(envelope.Signatures) <= maxSignatures {
		envelope.Signatures = append(envelope.Signatures, envelope.Signatures[0])
	}
	if _, err := trust(t, key).Verify(envelope); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want ErrInvalidSignature", err)
	}
}

func TestEmptyTrustSetVerifiesNothing(t *testing.T) {
	key := testKey(t)
	m, _ := testManifest(t)
	envelope, err := Sign(key, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := (TrustSet{}).Verify(envelope); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want ErrInvalidSignature", err)
	}
}

func TestParseEnvelopeRejectsBadInput(t *testing.T) {
	for name, input := range map[string][]byte{
		"not JSON":      []byte("nope"),
		"unknown field": []byte(`{"payload":"","payloadType":"","signatures":[],"extra":1}`),
		"too large":     append([]byte(`{"payload":"`), bytes.Repeat([]byte("a"), MaxEnvelopeBytes)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEnvelope(input); err == nil {
				t.Fatal("parsed bad input")
			}
		})
	}
}

func TestKeysRoundTripThroughPEM(t *testing.T) {
	key := testKey(t)
	encoded, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	parsed, err := ParsePrivateKey(encoded)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if !parsed.Equal(key) {
		t.Fatal("the parsed key differs")
	}

	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Fatal("parsed garbage as a private key")
	}
	public, err := MarshalPublicKey(key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}
	if _, err := ParsePrivateKey(public); err == nil {
		t.Fatal("parsed a public key as a private key")
	}
}

func TestParseTrustSetHoldsSeveralKeysForRotation(t *testing.T) {
	first, second := testKey(t), testKey(t)
	m, _ := testManifest(t)
	set := trust(t, first, second)
	for _, key := range []ed25519.PrivateKey{first, second} {
		envelope, err := Sign(key, m)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if _, err := set.Verify(envelope); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
}

func TestParseTrustSetRejectsBadInput(t *testing.T) {
	key := testKey(t)
	private, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	for name, input := range map[string][]byte{
		"empty":       nil,
		"garbage":     []byte("nope"),
		"private key": private,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTrustSet(input); err == nil {
				t.Fatal("parsed bad input")
			}
		})
	}
}

func TestChunkDigestChecks(t *testing.T) {
	m, content := testManifest(t)
	first := content[:m.ChunkLength(0)]
	if !m.ChunkMatches(0, first) {
		t.Fatal("a correct chunk didn't match")
	}
	corrupted := bytes.Clone(first)
	corrupted[10] ^= 0xff
	if m.ChunkMatches(0, corrupted) {
		t.Fatal("a corrupted chunk matched")
	}
	if m.ChunkMatches(0, first[:len(first)-1]) {
		t.Fatal("a truncated chunk matched")
	}
	if m.ChunkMatches(-1, first) || m.ChunkMatches(len(m.Chunks), first) {
		t.Fatal("an out-of-range chunk matched")
	}
}

func TestContentCheckRejectsAnythingButTheExactBytes(t *testing.T) {
	m, content := testManifest(t)
	if err := m.CheckContent(bytes.NewReader(content)); err != nil {
		t.Fatalf("CheckContent: %v", err)
	}
	corrupted := bytes.Clone(content)
	corrupted[5000] ^= 0xff
	for name, input := range map[string][]byte{
		"corrupted": corrupted,
		"truncated": content[:len(content)-1],
		"extended":  append(bytes.Clone(content), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.CheckContent(bytes.NewReader(input)); !errors.Is(err, ErrContentMismatch) {
				t.Fatalf("error = %v, want ErrContentMismatch", err)
			}
		})
	}
}
