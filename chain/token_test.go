package chain

import (
	"regexp"
	"testing"
)

func TestNewSecretIsThirtyTwoAlphanumerics(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		secret := NewSecret()
		if len(secret) != SecretLength {
			t.Fatalf("length %d, want %d: %q", len(secret), SecretLength, secret)
		}
		if !regexp.MustCompile(`^[0-9a-zA-Z]{32}$`).MatchString(secret) {
			t.Fatalf("alphabet: %q", secret)
		}
		if seen[secret] {
			t.Fatalf("repeated secret %q", secret)
		}
		seen[secret] = true
	}
}

func TestHashSecretIsLowercaseHex64(t *testing.T) {
	// Known sha256 vector: the hash must be the plain hex digest, because the
	// box computes it independently and compares strings.
	if got := HashSecret("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("HashSecret(abc) = %q", got)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, in := range []string{"", "abc", NewSecret(), "секрет"} {
		got := HashSecret(in)
		if !hex64.MatchString(got) {
			t.Fatalf("HashSecret(%q) = %q, want lowercase hex of length 64", in, got)
		}
	}
	if HashSecret("a") == HashSecret("b") {
		t.Fatal("different secrets hashed to the same value")
	}
}

func TestNameValid(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"edge-a", true},
		{"inner-1", true},
		{"a", true},
		{"legacy", true},
		{"0", true},
		{"a-b-c-9", true},
		{"abcdefghijabcdefghijabcdefghij12", true},   // 32 chars
		{"abcdefghijabcdefghijabcdefghij123", false}, // 33 chars
		{"", false},
		{"Edge-A", false},
		{"edge_a", false},
		{"edge a", false},
		{"edge.a", false},
		{"край", false},
		{"edge-a\n", false},
	}
	for _, tt := range tests {
		if got := NameValid(tt.name); got != tt.want {
			t.Errorf("NameValid(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
