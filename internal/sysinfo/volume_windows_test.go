package sysinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder on a plain drive is on that drive's volume ("C:\"), as before; a path that doesn't
// exist yet resolves through its nearest existing folder.
func TestVolumeOfPlainDrive(t *testing.T) {
	tmp := t.TempDir()
	want := strings.ToUpper(filepath.VolumeName(tmp)) + `\`
	if got := volume(tmp); got != want {
		t.Fatalf("volume(%s) = %q, want %q", tmp, got, want)
	}
	if got := volume(filepath.Join(tmp, "not", "yet")); got != want {
		t.Fatalf("missing path: %q", got)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}
