package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockIsIdempotentAndRemovable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	_ = os.WriteFile(p, []byte("# Mine\n\nkeep this\n"), 0o644)
	for i := 0; i < 3; i++ {
		if err := upsertBlock(p, Instructions); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), begin) != 1 || !strings.Contains(string(b), "keep this") {
		t.Fatalf("block duplicated or user text lost:\n%s", b)
	}
	_ = os.WriteFile(p, append(b, []byte("\nafter\n")...), 0o644)
	if err := upsertBlock(p, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "firefighter") || !strings.Contains(string(b), "keep this") || !strings.Contains(string(b), "after") {
		t.Fatalf("remove failed:\n%s", b)
	}
}

func TestBrokenMarkersLeaveFileAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	orig := "# Mine\n" + begin + "\nold rules\n\nmy own rules below a deleted end marker\n"
	_ = os.WriteFile(p, []byte(orig), 0o644)
	if err := upsertBlock(p, Instructions); err == nil {
		t.Fatal("a begin marker without an end must be an error")
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Fatalf("file changed:\n%s", b)
	}
}

func TestCodexTOMLIgnoresCommentedHeader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	p := filepath.Join(dir, "config.toml")
	orig := "# [mcp_servers.firefighter]\n[mcp_servers.other]\ncommand = \"y\"\n"
	_ = os.WriteFile(p, []byte(orig), 0o644)
	if err := codexTOML("/bin/ff", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "[mcp_servers.other]\ncommand = \"y\"") || !strings.Contains(string(b), "# [mcp_servers.firefighter]") {
		t.Fatalf("other server damaged:\n%s", b)
	}
	if _, err := os.Stat(p + ".firefighter.bak"); err != nil {
		t.Fatal("no backup written")
	}
}

func TestJSONServersKeepsOtherKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(p, []byte(`{"theme":"dark","mcpServers":{"other":{"command":"x"}}}`), 0o644)
	if err := jsonServers(p, "mcpServers", "/bin/ff", true); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	_ = json.Unmarshal(b, &m)
	servers := m["mcpServers"].(map[string]any)
	if m["theme"] != "dark" || servers["other"] == nil || servers["firefighter"] == nil {
		t.Fatalf("lost keys: %s", b)
	}
	_ = jsonServers(p, "mcpServers", "", false)
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "firefighter") || !strings.Contains(string(b), "other") {
		t.Fatalf("remove: %s", b)
	}
	if err := jsonServers(filepath.Join(t.TempDir(), "bad.json"), "mcpServers", "x", false); err != nil {
		t.Fatal("removing from a missing file must be a no-op")
	}
	bad := filepath.Join(t.TempDir(), "broken.json")
	_ = os.WriteFile(bad, []byte("{nope"), 0o644)
	if err := jsonServers(bad, "mcpServers", "x", true); err == nil {
		t.Fatal("must refuse to overwrite invalid JSON")
	}
}

func TestCodexTOML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	p := filepath.Join(dir, "config.toml")
	_ = os.WriteFile(p, []byte("model = \"x\"\n\n[mcp_servers.other]\ncommand = \"y\"\n"), 0o644)
	for i := 0; i < 2; i++ {
		if err := codexTOML(`C:\bin\firefighter.exe`, true); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "[mcp_servers.firefighter]") != 1 || !strings.Contains(string(b), "[mcp_servers.other]") {
		t.Fatalf("codex config:\n%s", b)
	}
	_ = codexTOML("", false)
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "firefighter") || !strings.Contains(string(b), "model") {
		t.Fatalf("codex remove:\n%s", b)
	}
}
