package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// T3 Code runs some agents with homes of their own: Antigravity always gets a private GEMINI_HOME
// per provider instance (providers/antigravity/<hash>), and Claude or Codex instances may have a
// homePath (CLAUDE_CONFIG_DIR / CODEX_HOME). Agents T3 starts there read neither ~/.gemini nor
// ~/.claude, so each of those homes gets the rules and the MCP server too.
func t3Agents() []agent {
	state := os.Getenv("T3CODE_HOME")
	if state == "" {
		state = filepath.Join(home(), ".t3", "userdata")
	}
	var out []agent
	profiles, _ := filepath.Glob(filepath.Join(state, "providers", "antigravity", "*"))
	for _, p := range profiles {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, agent{name: "T3 Code Antigravity", dirs: []string{p},
			mcp:   jsonMCP(filepath.Join(p, "config", "mcp_config.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(p, "GEMINI.md")}})
	}
	var settings struct {
		ProviderInstances map[string]struct {
			Driver string `json:"driver"`
			Config struct {
				HomePath string `json:"homePath"`
			} `json:"config"`
		} `json:"providerInstances"`
	}
	b, err := os.ReadFile(filepath.Join(state, "settings.json"))
	if err != nil || json.Unmarshal(b, &settings) != nil {
		return out
	}
	for _, in := range settings.ProviderInstances {
		dir := in.Config.HomePath
		if strings.HasPrefix(dir, "~") {
			dir = filepath.Join(home(), dir[1:])
		}
		if dir == "" || filepath.Clean(dir) == filepath.Clean(claudeDir()) || filepath.Clean(dir) == filepath.Clean(codexDir()) {
			continue // the usual home: setup covers it already
		}
		switch in.Driver {
		case "claudeAgent":
			out = append(out, agent{name: "T3 Code Claude (" + filepath.Base(dir) + ")", dirs: []string{dir},
				mcp:   jsonMCP(filepath.Join(dir, ".claude.json"), "mcpServers", stdio),
				rules: []string{filepath.Join(dir, "CLAUDE.md")}})
		case "codex":
			d := dir
			out = append(out, agent{name: "T3 Code Codex (" + filepath.Base(dir) + ")", dirs: []string{dir},
				mcp:   func(exe string, add bool) error { return tomlServers(filepath.Join(d, "config.toml"), exe, add) },
				rules: []string{filepath.Join(dir, "AGENTS.md")}})
		}
	}
	return out
}
