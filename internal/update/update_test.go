package update

import (
	"context"
	"crypto/sha256"
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
// matches it or not.
func release(t *testing.T, tag string, body []byte, goodSum bool) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				tag, AssetName(), srv.URL+"/a", srv.URL+"/sums")
		case "/a":
			_, _ = w.Write(body)
		case "/sums":
			sum := sha256.Sum256(body)
			if !goodSum {
				sum[0] ^= 0xff
			}
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), AssetName())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FIREFIGHTER_HOME", t.TempDir()) // the test server is only honored outside the real home
	t.Setenv("FIREFIGHTER_UPDATE_API", srv.URL)
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
	if err := Apply(context.Background(), rel, exe, func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a wrong checksum must stop the update: %v", err)
	}
	untouched("wrong checksum")

	release(t, "v9.9.9", []byte("matches its checksum but is no archive"), true)
	rel, _ = Latest(context.Background())
	if err := Apply(context.Background(), rel, exe, func(string) {}); err == nil {
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
