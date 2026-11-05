package auth

import (
	"strings"
	"testing"
)

func TestNewDeviceTokenIsUniqueAndMatchesItsHash(t *testing.T) {
	first, firstHash, err := NewDeviceToken()
	if err != nil {
		t.Fatalf("NewDeviceToken: %v", err)
	}
	second, _, err := NewDeviceToken()
	if err != nil {
		t.Fatalf("NewDeviceToken: %v", err)
	}
	if first == second {
		t.Fatal("two generated tokens are identical")
	}
	if HashToken(first) != firstHash {
		t.Fatal("returned hash doesn't match the token")
	}
	if !strings.HasPrefix(first, devicePrefix) {
		t.Fatalf("token %q lacks its prefix", first)
	}
	// 32 random bytes in unpadded base64url.
	if got := len(first) - len(devicePrefix); got != 43 {
		t.Fatalf("token body is %d characters, want 43", got)
	}
}

func TestAdminVerifier(t *testing.T) {
	verify := NewAdminVerifier("correct-horse-battery-staple")
	cases := map[string]bool{
		"Bearer correct-horse-battery-staple":  true,
		"Bearer correct-horse-battery-stapl":   false,
		"Bearer correct-horse-battery-staple!": false,
		"bearer correct-horse-battery-staple":  false,
		"correct-horse-battery-staple":         false,
		"Basic correct-horse-battery-staple":   false,
		"Bearer ":                              false,
		"":                                     false,
	}
	for header, want := range cases {
		if got := verify.Verify(header); got != want {
			t.Errorf("Verify(%q) = %v, want %v", header, got, want)
		}
	}
}
