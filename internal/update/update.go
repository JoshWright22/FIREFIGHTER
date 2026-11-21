// Package update finds a newer firefighter release on GitHub, checks it, and puts it in place of
// the running program. The guard does this on its own (update.auto); `firefighter update` does it
// on request.
//
// What is trusted: the release comes from this project's repository over HTTPS, the archive must
// match the release's checksums.txt, and when the GitHub CLI is installed and signed in it must
// also carry signed build provenance from this repository's release workflow (a release without
// it is refused). The new program is run once before it replaces anything.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Repo is the only place updates come from.
const Repo = "JoshWright22/FIREFIGHTER"

const module = "github.com/JoshWright22/firefighter/cmd/firefighter"

// Release is one published version.
type Release struct {
	Tag    string            // "v0.2.0"
	Assets map[string]string // file name -> download URL
}

// api is where releases are listed. FIREFIGHTER_UPDATE_API points tests at their own server; like
// scripted readings, it only works with a separate FIREFIGHTER_HOME, so nothing can point the real
// guard at another source of programs.
func api() (base string, test bool) {
	if u := os.Getenv("FIREFIGHTER_UPDATE_API"); u != "" && !config.IsDefaultHome() && loopback(u) {
		return strings.TrimRight(u, "/"), true
	}
	return "https://api.github.com", false
}

// loopback: the test override may only name a server on this machine. It skips provenance, and the
// program it replaces is the one every home shares, so content an agent reads must not be able to
// point it at a host on the internet (review round 6).
func loopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

var httpc = &http.Client{Timeout: 5 * time.Minute}

func get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "firefighter-update")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s: larger than %d MB", url, max>>20)
	}
	return b, err
}

// Latest asks GitHub for the newest release.
func Latest(ctx context.Context) (Release, error) {
	base, _ := api()
	b, err := get(ctx, base+"/repos/"+Repo+"/releases/latest", 4<<20)
	if err != nil {
		return Release{}, err
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, err
	}
	rel := Release{Tag: r.Tag, Assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	if !validTag(rel.Tag) {
		return Release{}, fmt.Errorf("release tag %q isn't a version", rel.Tag)
	}
	return rel, nil
}

// validTag: v1.2.3, digits and dots only (the tag ends up in a command line).
func validTag(t string) bool {
	if !strings.HasPrefix(t, "v") || len(t) < 2 || len(t) > 32 {
		return false
	}
	for _, c := range t[1:] {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// Newer reports whether version a is newer than b ("v1.2.3" or "1.2.3"). A development build
// ("dev") is never newer or older than anything: it is never replaced.
func Newer(a, b string) bool {
	if a == "dev" || b == "dev" || a == "" || b == "" {
		return false
	}
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// AssetName is this platform's archive in a release.
func AssetName() string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "firefighter_" + runtime.GOOS + "_" + runtime.GOARCH + ext
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "firefighter.exe"
	}
	return "firefighter"
}

// Apply downloads release rel, checks it, and puts its program at exe (the path of the running
// one). say gets one line per step. The old program stays beside it as firefighter.exe.old-<time>
// on Windows (a running program can be renamed, not overwritten) and is removed by Cleanup later.
func Apply(ctx context.Context, rel Release, exe string, say func(string)) error {
	name := AssetName()
	url, sums := rel.Assets[name], rel.Assets["checksums.txt"]
	if url == "" || sums == "" {
		return fmt.Errorf("release %s has no %s or no checksums.txt", rel.Tag, name)
	}
	dir := filepath.Dir(exe)
	tmp, err := os.MkdirTemp(dir, ".firefighter-update-") // beside the program: the final rename stays on one drive
	if err != nil {
		return fmt.Errorf("can't write next to %s: %w", exe, err)
	}
	defer os.RemoveAll(tmp)
	archive, err := get(ctx, url, 200<<20)
	if err != nil {
		return err
	}
	sumFile, err := get(ctx, sums, 1<<20)
	if err != nil {
		return err
	}
	got := sha256.Sum256(archive)
	if want := checksum(string(sumFile), name); want == "" || want != hex.EncodeToString(got[:]) {
		return fmt.Errorf("checksum mismatch for %s", name)
	}
	archivePath := filepath.Join(tmp, name)
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return err
	}
	if _, test := api(); !test {
		switch provenance(ctx, archivePath) {
		case "verified":
			say("signed build provenance verified")
		case "skipped":
			say("checksum verified (install and sign in to the GitHub CLI to also check signed build provenance)")
		default:
			return fmt.Errorf("release %s: %s has no valid signed build provenance; not installing it", rel.Tag, name)
		}
	}
	next := filepath.Join(tmp, exeName())
	if err := extract(archive, name, next); err != nil {
		return err
	}
	want := strings.TrimPrefix(rel.Tag, "v")
	if !runs(ctx, next, want) {
		// Antivirus blocked or removed it (Microsoft Defender's machine-learning rules do that to
		// new unsigned programs). The same release built here from its tagged source is left alone.
		say("the downloaded program was blocked from running (antivirus); building " + rel.Tag + " from source instead")
		if err := build(ctx, rel.Tag, tmp); err != nil {
			return fmt.Errorf("the downloaded firefighter was blocked, and building it from source failed: %w", err)
		}
		if !runs(ctx, next, want) {
			return errors.New("the firefighter built from source doesn't run either")
		}
	}
	old := ""
	if runtime.GOOS == "windows" {
		old = exe + ".old-" + time.Now().Format("20060102150405")
		if err := os.Rename(exe, old); err != nil {
			return err
		}
	}
	if err := os.Rename(next, exe); err != nil {
		if old != "" { // put the running guard's program back: never leave no firefighter.exe (review 11/20)
			if rerr := os.Rename(old, exe); rerr != nil {
				return fmt.Errorf("%w; and restoring %s failed: %v (the previous program is at %s)", err, exe, rerr, old)
			}
		}
		return err
	}
	say("installed " + rel.Tag + " at " + exe)
	return nil
}

// checksum finds name's SHA-256 in a checksums.txt ("<hex>  <name>" per line).
func checksum(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// provenance checks the archive's signed build attestation with the GitHub CLI: "verified",
// "failed", or "skipped" when the CLI isn't installed or signed in.
func provenance(ctx context.Context, archive string) string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "skipped"
	}
	run := func(args ...string) error {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cmd := exec.CommandContext(c, gh, args...)
		sysinfo.PrepareBackground(cmd)
		return cmd.Run()
	}
	if run("auth", "status") != nil {
		return "skipped"
	}
	if run("attestation", "verify", archive, "--repo", Repo) != nil {
		return "failed"
	}
	return "verified"
}

// extract writes the firefighter program inside the archive to dst.
func extract(archive []byte, name, dst string) error {
	write := func(r io.Reader) error {
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(r, 500<<20))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return err
		}
		for _, f := range zr.File {
			if isProgram(f.Name) {
				r, err := f.Open()
				if err != nil {
					return err
				}
				defer r.Close()
				return write(r)
			}
		}
		return fmt.Errorf("%s has no %s", name, exeName())
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("%s has no %s", name, exeName())
		}
		if h.Typeflag == tar.TypeReg && isProgram(h.Name) {
			return write(tr)
		}
	}
}

// isProgram says whether an archive entry is the firefighter program. Any case: v0.1.0 and v0.2.0
// were packed as FIREFIGHTER(.exe), after the repository's name.
func isProgram(entry string) bool {
	return !strings.ContainsAny(entry, `/\`) && strings.EqualFold(entry, exeName())
}

// runs says whether the program at path starts and reports the version expected.
func runs(ctx context.Context, path, version string) bool {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, path, "version")
	sysinfo.PrepareBackground(cmd)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "firefighter "+version
}

// build makes the release from its tagged source into dir, with the Go toolchain if there is one.
func build(ctx context.Context, tag, dir string) error {
	goExe, err := exec.LookPath("go")
	if err != nil {
		return errors.New("go isn't installed (https://go.dev/dl)")
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(c, goExe, "install", "-ldflags", "-X main.version="+strings.TrimPrefix(tag, "v"), module+"@"+tag)
	cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOFLAGS=")
	sysinfo.PrepareBackground(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Cleanup removes what earlier updates left beside the program: old copies nothing runs any more
// and unfinished downloads.
func Cleanup(exe string) {
	dir := filepath.Dir(exe)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, filepath.Base(exe)+".old-") || strings.HasPrefix(n, ".firefighter-update-") {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Minute {
				_ = os.RemoveAll(filepath.Join(dir, n)) // one still running can't be removed yet: next time
			}
		}
	}
}
