package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectOfWorktree(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "myrepo")
	os.MkdirAll(filepath.Join(main, ".git", "worktrees", "t3-abc"), 0o700)
	wt := filepath.Join(base, "worktrees", "t3-abc")
	os.MkdirAll(filepath.Join(wt, "src"), 0o700)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.ToSlash(filepath.Join(main, ".git", "worktrees", "t3-abc"))+"\n"), 0o600)
	if got := ProjectOf(filepath.Join(wt, "src")); got != "myrepo" {
		t.Fatalf("worktree project %q, want myrepo", got)
	}
	if got := ProjectOf(main); got != "myrepo" {
		t.Fatalf("main checkout %q", got)
	}
	// a relative gitdir (git 2.48+ worktree.useRelativePaths), worktree inside the repo
	rel := filepath.Join(main, ".claude", "worktrees", "x")
	os.MkdirAll(rel, 0o700)
	os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: ../../../.git/worktrees/x\n"), 0o600)
	if got := ProjectOf(rel); got != "myrepo" {
		t.Fatalf("relative worktree %q, want myrepo", got)
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
