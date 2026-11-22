package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Registering the MCP server changes only its own entry: every other key keeps its place and its
// exact value (big numbers too), an unchanged file isn't rewritten, and every write leaves a
// timestamped backup (security audit 2026-11-22, M3).
func TestJSONEntryKeepsOrderAndBacksUp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.json")
	orig := `{
  "zeta": 1,
  "numStartups": 12345678901234567890,
  "mcpServers": {
    "other": {"command": "x"}
  },
  "alpha": [true, null, "s"]
}
`
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jsonServers(p, "mcpServers", `C:\ff\firefighter.exe`, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	iz, in, im, ia := strings.Index(s, `"zeta"`), strings.Index(s, `"numStartups"`), strings.Index(s, `"mcpServers"`), strings.Index(s, `"alpha"`)
	if !(iz < in && in < im && im < ia) || !strings.Contains(s, "12345678901234567890") {
		t.Fatalf("order or values changed:\n%s", s)
	}
	if io, iff := strings.Index(s, `"other"`), strings.Index(s, `"firefighter"`); io < 0 || iff < io {
		t.Fatalf("the entry goes after the existing servers:\n%s", s)
	}
	baks, _ := filepath.Glob(p + ".firefighter.bak-*")
	if len(baks) != 1 {
		t.Fatalf("want one timestamped backup, got %v", baks)
	}
	if err := jsonServers(p, "mcpServers", `C:\ff\firefighter.exe`, true); err != nil {
		t.Fatal(err)
	}
	if baks2, _ := filepath.Glob(p + ".firefighter.bak-*"); len(baks2) != 1 {
		t.Fatalf("an unchanged file must not be rewritten: %v", baks2)
	}
	if err := jsonServers(p, "mcpServers", "", false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "firefighter") || !strings.Contains(string(b), `"other"`) {
		t.Fatalf("removal:\n%s", b)
	}
}
