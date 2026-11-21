package main

// End-to-end storage claims, labels, and cleanup.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 11/20: agents manage the storage the human allots them: claim a space, see it, keep under it,
// release it. A project over its claim starts no new job until it frees space or claims more.
func TestStorageSpaces(t *testing.T) {
	e := newEnv(t, nil)
	work := filepath.Join(e.home, "work", "alpha") // the agent works in project alpha
	os.MkdirAll(work, 0o700)
	st := func(args ...string) (string, int) {
		c := e.cmd(nil, args...)
		c.Dir = work
		out, err := c.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	if out, code := st("storage", "claim", "--gb", "1", "--project", "alpha"); code == 0 || !strings.Contains(out, "no storage allotted") {
		t.Fatalf("claim without an allotment: %d\n%s", code, out)
	}
	if out, code := e.ff("config", "set", "storage.gb", "2"); code != 0 {
		t.Fatalf("allot: %s", out)
	}
	out, code := st("storage", "claim", "alpha", "--gb", "0.1", "--why", "test data")
	if code != 0 || !strings.Contains(out, "0.1 GB space at") {
		t.Fatalf("claim: %d\n%s", code, out)
	}
	dir, _ := st("storage", "path", "--project", "alpha")
	dir = strings.TrimSpace(dir)
	if !strings.HasPrefix(dir, filepath.Join(e.home, "agent-storage")) {
		t.Fatalf("space %q should be inside the test's home", dir)
	}
	if out, code := st("storage", "claim", "beta", "--gb", "1.95"); code == 0 || !strings.Contains(out, "another project's space") {
		t.Fatalf("claim for another project without --force: %d\n%s", code, out)
	}
	if out, code := st("storage", "claim", "beta", "--gb", "2", "--force"); code == 0 || !strings.Contains(out, "1.9 GB of the 2.0 GB allotment is unclaimed") {
		t.Fatalf("over-allotment claim: %d\n%s", code, out)
	}
	// 0.2 GB in a 0.1 GB space: alpha's next job waits
	f, err := os.Create(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(200 << 20)
	f.Close()
	eventually(t, "the guard measured alpha's space", 20*time.Second, func() bool {
		return strings.Contains(fmt.Sprint(e.status()["notes"]), "over its 0.1 GB claim")
	})
	id := e.detach("exit:0", "--minutes", "1", "--project", "alpha")
	var line string
	eventually(t, "alpha's job held for storage", 20*time.Second, func() bool {
		line, _ = e.ff("wait", id, "--timeout", "1")
		return strings.Contains(line, "over its 0.1 GB claim")
	})
	if out, _ := st("storage"); !strings.Contains(out, "OVER by 0.1 GB") || !strings.Contains(out, "(test data)") {
		t.Fatalf("show:\n%s", out)
	}
	if out, code := st("storage", "claim", "alpha", "--gb", "0.5"); code != 0 {
		t.Fatalf("grow: %s", out)
	}
	if out, code := e.ff("wait", id, "--timeout", "30"); code != 0 || !strings.Contains(out, `"outcome": "ok"`) {
		t.Fatalf("after claiming more the job should run: %d\n%s", code, out)
	}
	if out, code := st("storage", "release", "alpha"); code == 0 || !strings.Contains(out, "still holds") {
		t.Fatalf("release with files: %d\n%s", code, out)
	}
	// rank what's there, then free the least important
	os.WriteFile(filepath.Join(dir, "results.json"), []byte("{}"), 0o600)
	if out, code := st("storage", "label", "big.bin", "--rank", "scratch", "--for", "shards, rebuilt each run", "--project", "alpha"); code != 0 {
		t.Fatalf("label: %s", out)
	}
	if out, code := st("storage", "label", "results.json", "--rank", "keep", "--for", "final numbers", "--project", "alpha"); code != 0 {
		t.Fatalf("label: %s", out)
	}
	if out, _ := st("storage", "items", "--project", "alpha"); strings.Index(out, "scratch") > strings.Index(out, "keep") || !strings.Contains(out, "final numbers") {
		t.Fatalf("items, least important first:\n%s", out)
	}
	if out, code := st("storage", "free", "--gb", "1", "--upto", "useful", "--project", "alpha"); code != 0 || !strings.Contains(out, `deleted "big.bin" (scratch`) || strings.Contains(out, "results.json") {
		t.Fatalf("free: %d\n%s", code, out)
	}
	if !exists(filepath.Join(dir, "results.json")) || exists(filepath.Join(dir, "big.bin")) {
		t.Fatal("free must delete scratch and never keep")
	}
	if out, code := st("storage", "release", "alpha", "--delete"); code == 0 || !strings.Contains(out, "ranked keep") {
		t.Fatalf("release --delete over a keep file must refuse: %d\n%s", code, out)
	}
	if out, code := st("storage", "label", "results.json", "--rank", "none", "--project", "alpha"); code != 0 {
		t.Fatalf("unrank: %s", out)
	}
	if out, code := st("storage", "release", "alpha", "--delete"); code != 0 || !strings.Contains(out, "space released") {
		t.Fatalf("release --delete: %d\n%s", code, out)
	}
	if exists(dir) {
		t.Fatal("space folder still there")
	}
}
