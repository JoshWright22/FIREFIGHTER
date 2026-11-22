package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v0.2.0", "0.1.0", true}, {"v0.2.0", "v0.2.0", false}, {"v0.1.9", "0.2.0", false}, {"v1.0.0", "0.9.9", true},
		{"v0.2.0", "dev", false}, {"dev", "0.1.0", false}, {"v0.10.0", "0.9.0", true}} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestTagsAndChecksums(t *testing.T) {
	for tag, ok := range map[string]bool{"v0.2.0": true, "v1.2.3": true, "0.2.0": false, "v1; rm -rf /": false, "v": false, "latest": false} {
		if validTag(tag) != ok {
			t.Errorf("validTag(%q) != %v", tag, ok)
		}
	}
	sums := "aaa  firefighter_linux_amd64.tar.gz\nBBB  firefighter_windows_amd64.zip\n"
	if checksum(sums, "firefighter_windows_amd64.zip") != "bbb" || checksum(sums, "other.zip") != "" {
		t.Fatal("checksum lookup")
	}
}

// release serves a fake GitHub with one release whose archive is body, and a checksums.txt that
// matches it or not, signed with a key the test trusts.
func release(t *testing.T, tag string, body []byte, goodSum bool) {
	t.Helper()
	signedRelease(t, tag, body, goodSum, "good")
}

// signedRelease: sig is "good" (signed with the trusted key), "none" (no .sig asset) or "other"
// (signed with a key nobody trusts).
func signedRelease(t *testing.T, tag string, body []byte, goodSum bool, sig string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	sumsOf := func() []byte {
		sum := sha256.Sum256(body)
		if !goodSum {
			sum[0] ^= 0xff
		}
		return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), AssetName()))
	}
	sigAsset := ""
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}%s]}`,
				tag, AssetName(), srv.URL+"/a", srv.URL+"/sums", sigAsset)
		case "/sig":
			key := priv
			if sig == "other" {
				key = other
			}
			fmt.Fprintln(w, base64.StdEncoding.EncodeToString(ed25519.Sign(key, sumsOf())))
		case "/a":
			_, _ = w.Write(body)
		case "/sums":
			_, _ = w.Write(sumsOf())
		default:
			http.NotFound(w, r)
		}
	}))
	if sig != "none" {
		sigAsset = fmt.Sprintf(`,{"name":%q,"browser_download_url":%q}`, SigAsset, srv.URL+"/sig")
	}
	t.Cleanup(srv.Close)
	t.Setenv("FIREFIGHTER_HOME", t.TempDir()) // the test server is only honored outside the real home
	t.Setenv("FIREFIGHTER_UPDATE_API", srv.URL)
	t.Setenv("FIREFIGHTER_UPDATE_TEST_KEY", base64.StdEncoding.EncodeToString(pub))
}

// A release installs only with a signature from a trusted key: none, or another key's, and
// nothing changes (security audit H1), whether or not the GitHub CLI is around.
func TestApplyRefusesAnUnsignedRelease(t *testing.T) {
	for _, sig := range []string{"none", "other"} {
		dir := t.TempDir()
		exe := filepath.Join(dir, exeName())
		_ = os.WriteFile(exe, []byte("the running program"), 0o755)
		signedRelease(t, "v9.9.9", []byte("whatever"), true, sig)
		rel, err := Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		err = Apply(context.Background(), rel, exe, func(string) {}, Options{})
		if err == nil || !strings.Contains(err.Error(), SigAsset) {
			t.Fatalf("%s: an update without a trusted signature must be refused: %v", sig, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "the running program" {
			t.Fatalf("%s: the program was replaced", sig)
		}
	}
	// And a build that trusts no key installs nothing at all.
	signedRelease(t, "v9.9.9", []byte("whatever"), true, "good")
	t.Setenv("FIREFIGHTER_UPDATE_TEST_KEY", "")
	rel, _ := Latest(context.Background())
	if err := Apply(context.Background(), rel, filepath.Join(t.TempDir(), exeName()), func(string) {}, Options{}); err != ErrNoKey {
		t.Fatalf("no trusted key: %v", err)
	}
}

// An archive that doesn't match the release's checksum, or holds no program, changes nothing.
func TestApplyRefusesWhatDoesNotCheckOut(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName())
	if err := os.WriteFile(exe, []byte("the running program"), 0o755); err != nil {
		t.Fatal(err)
	}
	untouched := func(why string) {
		t.Helper()
		if b, _ := os.ReadFile(exe); string(b) != "the running program" {
			t.Fatalf("%s: the program was replaced", why)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != 1 {
			t.Fatalf("%s: leftovers beside the program: %v", why, ents)
		}
	}
	release(t, "v9.9.9", []byte("not what was published"), false)
	rel, err := Latest(context.Background())
	if err != nil || rel.Tag != "v9.9.9" || rel.Assets[AssetName()] == "" {
		t.Fatalf("Latest: %+v, %v", rel, err)
	}
	if err := Apply(context.Background(), rel, exe, func(string) {}, Options{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a wrong checksum must stop the update: %v", err)
	}
	untouched("wrong checksum")

	release(t, "v9.9.9", []byte("matches its checksum but is no archive"), true)
	rel, _ = Latest(context.Background())
	if err := Apply(context.Background(), rel, exe, func(string) {}, Options{}); err == nil {
		t.Fatal("a broken archive must stop the update")
	}
	untouched("broken archive")
}

// Outside tests the release list always comes from GitHub: the override is ignored in the real home.
func TestOverrideNeedsItsOwnHome(t *testing.T) {
	t.Setenv("FIREFIGHTER_HOME", "")
	t.Setenv("FIREFIGHTER_UPDATE_API", "http://127.0.0.1:1")
	if base, test := api(); test || base != "https://api.github.com" {
		t.Fatalf("api() = %s, %v: the real guard must only ask GitHub", base, test)
	}
}

// v0.2.0's archives hold the program as FIREFIGHTER(.exe), after the repository's name, and the
// updater looked for firefighter(.exe) by exact name: "has no firefighter.exe". Any case is found;
// a file of that name in a folder is not.
func TestExtractFindsTheProgramWhateverItsCase(t *testing.T) {
	upper := strings.ToUpper(strings.TrimSuffix(exeName(), ".exe"))
	if strings.HasSuffix(exeName(), ".exe") {
		upper += ".exe"
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{"LICENSE": "mit", "docs/" + exeName(): "decoy", upper: "the program"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{{"README.md", "readme"}, {upper, "the program"}} {
		_ = tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o755, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	_ = gz.Close()
	for name, archive := range map[string][]byte{"x.zip": zb.Bytes(), "x.tar.gz": tb.Bytes()} {
		dst := filepath.Join(t.TempDir(), exeName())
		if err := extract(archive, name, dst); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, _ := os.ReadFile(dst); string(b) != "the program" {
			t.Fatalf("%s: extracted %q", name, b)
		}
	}
}

// Review round 6 (Fable): the test override never names a host off this machine.
func TestOverrideIsLoopbackOnly(t *testing.T) {
	for raw, want := range map[string]bool{"http://127.0.0.1:5555": true, "http://localhost:1": true, "http://[::1]:9": true,
		"http://evil.example": false, "https://example.com": false, "http://127.0.0.1.evil.example": false, "::bad": false} {
		if loopback(raw) != want {
			t.Errorf("loopback(%q) != %v", raw, want)
		}
	}
}
