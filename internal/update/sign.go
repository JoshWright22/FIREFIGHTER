package update

// Release signing (security audit 2026-11-22, H1): checksums.txt comes from the same release as the
// archive, so it only catches corruption, and build provenance only proves "built by our workflow
// from some tag", which anyone (or any agent) who can push a v* tag gets. A release installs only
// when its checksums.txt carries an ed25519 signature from a key below, made offline with
// `go run ./scripts/releasekey sign` and a passphrase no agent knows. Fails closed: no key, no
// signature, or a bad one, and nothing is installed, with or without the GitHub CLI.

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SigAsset is the release asset holding the signature of checksums.txt (base64, one line).
const SigAsset = "checksums.txt.sig"

// releaseKeys are the public keys (base64) trusted to sign releases. Empty until the maintainer
// makes one (`go run ./scripts/releasekey gen`): until then no release installs itself.
var releaseKeys = []string{}

func trustedKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	list := releaseKeys
	if _, test := api(); test {
		// Tests sign their own fake releases; only with the loopback test server in a separate home.
		list = append(append([]string{}, list...), strings.Fields(os.Getenv("FIREFIGHTER_UPDATE_TEST_KEY"))...)
	}
	for _, k := range list {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k)); err == nil && len(b) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(b))
		}
	}
	return out
}

// ErrNoKey: this build trusts no signing key, so it installs no release.
var ErrNoKey = errors.New("this build trusts no release-signing key, so it installs no release by itself (docs/security.md: release signing)")

// verifySums checks sig (the SigAsset's content) over sums against the trusted keys.
func verifySums(tag string, sums, sig []byte) error {
	keys := trustedKeys()
	if len(keys) == 0 {
		return ErrNoKey
	}
	if len(sig) == 0 {
		return fmt.Errorf("release %s has no %s: an unsigned release is not installed", tag, SigAsset)
	}
	raw := sig
	if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig))); err == nil {
		raw = b
	}
	for _, k := range keys {
		if len(raw) == ed25519.SignatureSize && ed25519.Verify(k, sums, raw) {
			return nil
		}
	}
	return fmt.Errorf("release %s: %s doesn't match any trusted release key; not installing it", tag, SigAsset)
}

// Trusts says whether this build trusts any release key (none: no update can install).
func Trusts() bool { return len(trustedKeys()) > 0 }
