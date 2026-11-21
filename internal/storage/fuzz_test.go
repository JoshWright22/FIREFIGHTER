package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// Agent-supplied project names: whatever comes in, the space name is safe to put under the root.
func FuzzName(f *testing.F) {
	for _, s := range []string{"My Project", "alpha.", "CON", "nul.txt", "../x", `a\b`, "", "....", "-x", "ü", strings.Repeat("a", 200)} {
		f.Add(s)
	}
	p := Pool{Root: filepath.Join(f.TempDir(), "root"), GB: 1}
	f.Fuzz(func(t *testing.T, in string) {
		n := Name(in)
		if n == "" {
			return
		}
		if Name(n) != n {
			t.Fatalf("Name not idempotent: %q -> %q -> %q", in, n, Name(n))
		}
		if strings.ContainsAny(n, `/\:`) || n == "." || n == ".." || strings.HasSuffix(n, ".") || reserved.MatchString(n) {
			t.Fatalf("unsafe space name %q from %q", n, in)
		}
		dir, err := p.Dir(n)
		if err != nil {
			t.Fatalf("Name(%q) = %q, which Dir refuses: %v", in, n, err)
		}
		if filepath.Dir(dir) != filepath.Clean(p.Root) {
			t.Fatalf("space %q is not directly under the root", dir)
		}
	})
}

// Agent-supplied label paths never leave the space.
func FuzzRelPath(f *testing.F) {
	for _, s := range []string{"data", "a/b/c", "../x", "..", ".", "/etc/passwd", `C:\Windows`, "a/../../b", "a\\..\\..\\b", ""} {
		f.Add(s)
	}
	dir := filepath.Join(f.TempDir(), "space")
	f.Fuzz(func(t *testing.T, in string) {
		rel, err := relPath(dir, in)
		if err != nil {
			return
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		r, err := filepath.Rel(dir, full)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			t.Fatalf("relPath(%q) = %q escapes the space (%s)", in, rel, full)
		}
	})
}
