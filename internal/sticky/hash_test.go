package sticky

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

func TestDeriveGoldenVector(t *testing.T) {
	sum := sha256.Sum256([]byte("saltkey"))
	want := hex.EncodeToString(sum[:])
	if got := Derive("salt", "key", 64); got != want {
		t.Errorf("Derive(salt,key,64)=%q want full sha256 hex %q", got, want)
	}
	if got := Derive("salt", "key", 16); got != "4a466ea0657e4795" {
		t.Errorf("golden 16-char derivation mismatch: %q", got)
	}
}

func TestDeriveDeterministic(t *testing.T) {
	a := Derive("salt-x", "Marker-123", 16)
	b := Derive("salt-x", "Marker-123", 16)
	if a != b {
		t.Fatalf("same input must yield same account: %q vs %q", a, b)
	}
}

func TestDeriveLengthClamp(t *testing.T) {
	cases := []struct{ sidLen, want int }{
		{-5, 16}, {0, 16}, {4, 4}, {16, 16}, {63, 63}, {64, 64}, {65, 64}, {1000, 64},
	}
	full := Derive("s", "k", 64)
	for _, c := range cases {
		got := Derive("s", "k", c.sidLen)
		if len(got) != c.want {
			t.Errorf("sidLen=%d produced length %d want %d", c.sidLen, len(got), c.want)
		}
		if !strings.HasPrefix(full, got) {
			t.Errorf("truncation must be a strict prefix of the full hash")
		}
	}
}

func TestDeriveOutputCharset(t *testing.T) {
	got := Derive("s", "k", 64)
	if strings.ToLower(got) != got {
		t.Error("account must be lowercase hex")
	}
	for _, ch := range got {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Errorf("unexpected char %q in account", ch)
		}
	}
}

func TestDeriveCaseSensitiveAndSaltSensitive(t *testing.T) {
	if Derive("s", "Marker", 64) == Derive("s", "mk", 64) {
		t.Error("API keys are case sensitive: different case must derive differently")
	}
	if Derive("salt-a", "Marker", 64) == Derive("salt-b", "Marker", 64) {
		t.Error("different salt must re-shuffle all accounts")
	}
}

func TestCombineSaltZeroKeepsHistoricalIdentity(t *testing.T) {
	if got := CombineSalt("sys-salt", 0); got != "sys-salt" {
		t.Fatalf("unrotated per-marker salt must return system salt verbatim, got %q", got)
	}
	before := Derive(CombineSalt("sys-salt", 0), "Marker", 32)
	if before != Derive("sys-salt", "Marker", 32) {
		t.Error("historical compatibility broken: unrotated derivation changed")
	}
}

func TestCombineSaltRotatedChangesIdentity(t *testing.T) {
	base := Derive(CombineSalt("sys", 0), "Marker", 32)
	for k := int64(1); k <= 3; k++ {
		salt := CombineSalt("sys", k)
		want := "sys#k" + strconv.FormatInt(k, 10)
		if salt != want {
			t.Fatalf("CombineSalt(sys,%d)=%q want %q", k, salt, want)
		}
		if Derive(salt, "Marker", 32) == base {
			t.Errorf("rotation k=%d must change the derived identity", k)
		}
	}
	if CombineSalt("", 7) != "#k7" {
		t.Error("empty system salt should still gain rotation suffix")
	}
}

func TestFingerprint(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-test"))
	want := hex.EncodeToString(sum[:])[:8]
	if got := Fingerprint("sk-test"); got != want {
		t.Errorf("Fingerprint=%q want %q", got, want)
	}
	if len(Fingerprint("x")) != 8 {
		t.Error("fingerprint must be exactly 8 hex chars")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Error("distinct keys should not share fingerprint (sanity)")
	}
}
