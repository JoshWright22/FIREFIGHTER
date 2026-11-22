package harvest

// Approved harvester commands (security audit 2026-11-22, M2): the harvester's start, stop and
// running commands are shell lines the guard runs at logon and whenever the GPU goes idle, outside
// any agent's sandbox. firefighter.toml alone is not enough to run one: its SHA-256 must also be in
// harvester-approved, which only `firefighter harvest approve` writes, from the human's own
// terminal, after showing the exact commands. Something that edits firefighter.toml (an agent, a
// script) can't quietly plant a command that runs at every logon.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// ApprovedPath is the list of approved command hashes, one per line.
func ApprovedPath(home string) string { return filepath.Join(home, "harvester-approved") }

func hash(cmd string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(cmd)))
	return hex.EncodeToString(h[:])
}

// Approved says whether cmd may run.
func Approved(home, cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return true
	}
	b, err := os.ReadFile(ApprovedPath(home))
	if err != nil {
		return false
	}
	want := hash(cmd)
	for _, l := range strings.Fields(string(b)) {
		if l == want {
			return true
		}
	}
	return false
}

// Approve replaces the approved list with exactly these commands.
func Approve(home string, cmds ...string) error {
	var lines []string
	for _, c := range cmds {
		if strings.TrimSpace(c) != "" {
			lines = append(lines, hash(c))
		}
	}
	tmp := ApprovedPath(home) + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ApprovedPath(home))
}
