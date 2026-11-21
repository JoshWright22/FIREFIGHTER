package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// worktree makes a linked worktree of main at dir the way git does: dir/.git names
// main/.git/worktrees/<name>, and that folder's gitdir file names dir/.git back.
func worktree(t *testing.T, main, dir, name string, relative bool) {
	t.Helper()
	meta := filepath.Join(main, ".git", "worktrees", name)
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	ptr := filepath.ToSlash(meta)
	if relative {
		r, _ := filepath.Rel(dir, meta)
		ptr = filepath.ToSlash(r)
	}
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+ptr+"\n"), 0o600)
	os.WriteFile(filepath.Join(meta, "gitdir"), []byte(filepath.Join(dir, ".git")+"\n"), 0o600)
}

func TestProjectOfWorktree(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "myrepo")
	os.MkdirAll(filepath.Join(main, ".git"), 0o700)

	wt := filepath.Join(base, "worktrees", "t3-abc")
	worktree(t, main, wt, "t3-abc", false)
	os.MkdirAll(filepath.Join(wt, "src"), 0o700)
	if got := ProjectOf(filepath.Join(wt, "src")); got != "myrepo" {
		t.Fatalf("worktree project %q, want myrepo", got)
	}
	if got := ProjectOf(main); got != "myrepo" {
		t.Fatalf("main checkout %q", got)
	}
	// a relative gitdir (git 2.48+ worktree.useRelativePaths), worktree inside the repo
	rel := filepath.Join(main, ".claude", "worktrees", "x")
	worktree(t, main, rel, "x", true)
	if got := ProjectOf(rel); got != "myrepo" {
		t.Fatalf("relative worktree %q, want myrepo", got)
	}
	// a made-up .git file pointing at another repo's worktree metadata (no back-pointer to it)
	fake := filepath.Join(base, "fake")
	os.MkdirAll(fake, 0o700)
	os.WriteFile(filepath.Join(fake, ".git"), []byte("gitdir: "+filepath.ToSlash(filepath.Join(main, ".git", "worktrees", "t3-abc"))+"\n"), 0o600)
	if got := ProjectOf(fake); got != "fake" {
		t.Fatalf("a .git file without git's back-pointer counted as %q, want its own folder", got)
	}
	sub := filepath.Join(main, "vendor", "lib")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../../.git/modules/lib\n"), 0o600)
	if got := ProjectOf(sub); got != "lib" {
		t.Fatalf("submodule %q, want its own name", got)
	}
	plain := filepath.Join(base, "notes")
	os.MkdirAll(plain, 0o700)
	if got := ProjectOf(plain); got != "notes" {
		t.Fatalf("plain folder %q", got)
	}
}
