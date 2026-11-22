package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key made with gen signs a checksums.txt that its public key verifies; the wrong passphrase
// signs nothing, and gen never overwrites a key.
func TestGenSignVerify(t *testing.T) {
	dir := t.TempDir()
	key, sums := filepath.Join(dir, "release.key"), filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(sums, []byte("abc  firefighter_windows_amd64.zip\n"), 0o644)
	t.Setenv("FIREFIGHTER_SIGN_PASSPHRASE", "correct horse battery staple")
	if err := run([]string{"gen", key}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"gen", key}); err == nil {
		t.Fatal("gen must not overwrite a key")
	}
	if err := run([]string{"sign", key, sums}); err != nil {
		t.Fatal(err)
	}
	var kf keyFile
	b, _ := os.ReadFile(key)
	_ = json.Unmarshal(b, &kf)
	pub, _ := base64.StdEncoding.DecodeString(kf.Public)
	sig, _ := os.ReadFile(sums + ".sig")
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	data, _ := os.ReadFile(sums)
	if !ed25519.Verify(pub, data, raw) {
		t.Fatal("signature doesn't verify")
	}
	if strings.Contains(string(b), base64.StdEncoding.EncodeToString(raw[:8])) {
		t.Fatal("unexpected key material")
	}
	t.Setenv("FIREFIGHTER_SIGN_PASSPHRASE", "the wrong passphrase!")
	if err := run([]string{"sign", key, sums}); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("wrong passphrase: %v", err)
	}
}
