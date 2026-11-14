package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/update"
)

func buildVersion(t *testing.T, version, out string) {
	t.Helper()
	if b, err := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", out, ".").CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, b)
	}
}

// archiveOf packs a program the way a release does (zip on Windows, tar.gz elsewhere).
func archiveOf(t *testing.T, exe string) []byte {
	t.Helper()
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	name := filepath.Base(exe)
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(name)
		_, _ = w.Write(body)
		_ = zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// A released guard updates itself: version 0.0.1 is running, a release 0.0.2 appears, and with no
// job running the guard downloads it, checks its checksum, puts it in place of its own program and
// comes back as 0.0.2, with nobody asking. A job that was running at the time finishes first.
func TestGuardUpdatesItself(t *testing.T) {
	bin, err := os.MkdirTemp("", "ff-update") // not t.TempDir: Windows may still hold the old program a moment
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bin) })
	name := "firefighter"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe := filepath.Join(bin, name)
	buildVersion(t, "0.0.1", exe)
	newDir := filepath.Join(bin, "new")
	_ = os.MkdirAll(newDir, 0o755)
	buildVersion(t, "0.0.2", filepath.Join(newDir, name))
	archive := archiveOf(t, filepath.Join(newDir, name))
	_ = os.RemoveAll(newDir)

	var published atomic.Bool // the release appears only once the test says so
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + update.Repo + "/releases/latest":
			tag := "v0.0.1"
			if published.Load() {
				tag = "v0.0.2"
			}
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				tag, update.AssetName(), srv.URL+"/archive", srv.URL+"/sums")
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			sum := sha256.Sum256(archive)
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), update.AssetName())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	e := newEnv(t, nil)
	e.vars = append(e.vars, "FIREFIGHTER_UPDATE_API="+srv.URL, "FIREFIGHTER_UPDATE_EVERY=1")
	run := func(args ...string) string {
		c := exec.Command(exe, args...)
		c.Env = e.vars
		out, _ := c.CombinedOutput()
		return string(out)
	}
	guard := func() string { // the running guard's version
		out := run("status", "--json")
		i := strings.Index(out, `"version": "`)
		if i < 0 {
			return ""
		}
		out = out[i+len(`"version": "`):]
		return out[:strings.Index(out, `"`)]
	}
	if v := guard(); v != "0.0.1" {
		t.Fatalf("guard version = %q, want 0.0.1", v)
	}
	// A job is running when the release appears: the update waits for it.
	log := filepath.Join(e.home, "job.txt")
	self, _ := os.Executable()
	job := exec.Command(exe, "run", "--minutes", "1", "--name", "busy", "--", self)
	job.Env = append(append([]string{}, e.vars...), "FF_HELPER=sleep:6000:"+log)
	if err := job.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "job running", 15*time.Second, func() bool { return strings.Contains(readFile(log), "start") })
	published.Store(true)
	time.Sleep(3 * time.Second)
	if v := guard(); v != "0.0.1" {
		t.Fatalf("the guard updated (to %q) while a job was running", v)
	}
	if out := run("status"); !strings.Contains(out, "v0.0.2 is out") {
		t.Fatalf("status doesn't mention the waiting update:\n%s", out)
	}
	if err := job.Wait(); err != nil || !strings.Contains(readFile(log), "end") {
		t.Fatalf("the job didn't finish cleanly: %v\n%s", err, readFile(log))
	}
	eventually(t, "the guard to come back as 0.0.2", 60*time.Second, func() bool { return guard() == "0.0.2" })
	if out := run("version"); strings.TrimSpace(out) != "firefighter 0.0.2" {
		t.Fatalf("the program on disk is %q, want 0.0.2", out)
	}
	if l := e.log(); !strings.Contains(l, "update: restarting as v0.0.2") {
		t.Fatalf("daemon.log:\n%s", l)
	}
	// And it still works: a job runs under the new guard.
	after := exec.Command(exe, "run", "--minutes", "1", "--name", "after", "--", self)
	after.Env = append(append([]string{}, e.vars...), "FF_HELPER=exit:0")
	if out, err := after.CombinedOutput(); err != nil || !strings.Contains(string(out), "helper-out") {
		t.Fatalf("a job under the updated guard: %v: %s", err, out)
	}
	_ = run("daemon", "--stop")
}
