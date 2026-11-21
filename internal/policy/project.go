package policy

import (
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
	b, err := os.ReadFile(gitFile)
	if err != nil || len(b) > 4096 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	p, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return ""
	}
	p = filepath.ToSlash(strings.TrimSpace(p))
	i := strings.LastIndex(p, "/.git/worktrees/")
	if i <= 0 {
		return "" // a submodule (.git/modules/...) or something else: the folder's own name
	}
	return filepath.FromSlash(p[:i])
}
