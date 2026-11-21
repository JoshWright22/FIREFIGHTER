package policy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ProjectOf names the project a folder is in: its git repo's name, else the folder's name. A linked
// worktree (its .git is a file: "gitdir: <main>/.git/worktrees/<name>", as agent orchestrators like
// T3 Code make one per task) is its main repo's project, so one repo's agents share one budget,
// one learned history and one storage space instead of one per checkout (review 11/20, after T3
// Code's stable project identity).
func ProjectOf(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil {
			if !fi.IsDir() {
				if main := worktreeMain(g); main != "" {
					return filepath.Base(main)
				}
			}
			return filepath.Base(d)
		}
		if filepath.Dir(d) == d {
			return filepath.Base(dir)
		}
	}
}

// worktreeMain is the main checkout of a linked worktree's .git file ("" if it isn't one).
func worktreeMain(gitFile string) string {
	b := readSmall(gitFile)
	if b == nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	p, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return ""
	}
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) { // git 2.48+ can write it relative to the .git file (worktree.useRelativePaths)
		p = filepath.Join(filepath.Dir(gitFile), p)
	}
	p = filepath.ToSlash(filepath.Clean(p))
	i := strings.LastIndex(p, "/.git/worktrees/")
	if i <= 0 {
		return "" // a submodule (.git/modules/...) or something else: the folder's own name
	}
	// git's own back-pointer: <main>/.git/worktrees/<name>/gitdir names this .git file. A stray or
	// made-up .git file doesn't, and then the folder's own name counts (review 11/20).
	back := readSmall(filepath.Join(filepath.FromSlash(p), "gitdir"))
	if back == nil {
		return ""
	}
	bp := strings.TrimSpace(string(back))
	if !filepath.IsAbs(bp) {
		bp = filepath.Join(filepath.FromSlash(p), bp)
	}
	if !sameFile(bp, gitFile) {
		return ""
	}
	main := filepath.FromSlash(p[:i])
	if b := filepath.Base(main); b == "." || b == ".." || b == string(filepath.Separator) || filepath.Dir(main) == main {
		return ""
	}
	return main
}

// readSmall reads a small regular file (nil for anything else: a FIFO or device would block, and a
// big file isn't git metadata).
func readSmall(path string) []byte {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil
	}
	return b
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}
