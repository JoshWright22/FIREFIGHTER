package setup

import (
	"os"
	"path/filepath"
	"runtime"
)

// More agents: everything T3 Code can drive (and the common ACP-registry agents), beyond Claude Code,
// Codex, Gemini CLI and Cursor, which setup.go handles itself. Each is only touched when its config
// folder exists (it's installed). Formats checked against each agent's docs or source, Nov 2026.
//
// MCP entry shapes differ: OpenCode and fx put the args inside one command array, Copilot wants
// type "local" and a tools list, Muse wants a transport and a schema_version, Amp keys servers
// under a dotted top-level "amp.mcpServers". Goose (YAML) and Aider (no MCP) get rules only.

type agent struct {
	name  string
	dirs  []string                         // installed if any exists
	mcp   func(exe string, add bool) error // nil: no MCP config we can write
	rules []string                         // global instruction files it reads in every session
}

func cfgHome() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".config")
}

func appData() string {
	if d := os.Getenv("APPDATA"); d != "" && runtime.GOOS == "windows" {
		return d
	}
	return cfgHome()
}

// stdio is the usual {"command": exe, "args": ["mcp"]} entry.
func stdio(exe string) map[string]any { return map[string]any{"command": exe, "args": []string{"mcp"}} }

func jsonMCP(path, key string, entry func(string) map[string]any) func(string, bool) error {
	return func(exe string, add bool) error {
		var e map[string]any
		if add {
			e = entry(exe)
		}
		return jsonEntry(path, key, e, add, nil)
	}
}

func agents() []agent {
	h := home()
	oc := filepath.Join(cfgHome(), "opencode")
	return []agent{
		{name: "Antigravity", dirs: []string{filepath.Join(h, ".gemini", "config"), filepath.Join(h, ".gemini", "antigravity")},
			mcp: jsonMCP(filepath.Join(h, ".gemini", "config", "mcp_config.json"), "mcpServers", stdio)},
		// rules: ~/.gemini/GEMINI.md, written for Gemini CLI, is read by Antigravity too
		{name: "Grok Build", dirs: []string{filepath.Join(h, ".grok")},
			mcp: func(exe string, add bool) error {
				return tomlServers(filepath.Join(h, ".grok", "config.toml"), exe, add)
			},
			rules: []string{filepath.Join(h, ".grok", "rules", "firefighter.md")}},
		{name: "OpenCode", dirs: []string{oc},
			mcp: jsonMCP(filepath.Join(oc, "opencode.json"), "mcp", func(exe string) map[string]any {
				return map[string]any{"type": "local", "command": []string{exe, "mcp"}, "enabled": true}
			}),
			rules: []string{filepath.Join(oc, "AGENTS.md")}},
		{name: "Kimi CLI", dirs: []string{filepath.Join(h, ".kimi")},
			mcp: jsonMCP(filepath.Join(h, ".kimi", "mcp.json"), "mcpServers", stdio)},
		{name: "Kimi Code", dirs: []string{filepath.Join(h, ".kimi-code")},
			mcp:   jsonMCP(filepath.Join(h, ".kimi-code", "mcp.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".kimi-code", "AGENTS.md")}},
		{name: "Pi", dirs: []string{filepath.Join(h, ".pi", "agent")},
			mcp:   jsonMCP(filepath.Join(h, ".pi", "agent", "mcp.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".pi", "agent", "AGENTS.md")}},
		{name: "Muse Code", dirs: []string{filepath.Join(cfgHome(), "muse")},
			mcp: func(exe string, add bool) error {
				var e map[string]any
				if add {
					e = map[string]any{"transport": "stdio", "command": exe, "args": []string{"mcp"}, "mode": "optional"}
				}
				return jsonEntry(filepath.Join(cfgHome(), "muse", "settings.json"), "mcp_servers", e, add, map[string]any{"schema_version": 1})
			}},
		{name: "fx", dirs: []string{filepath.Join(h, ".fx")},
			mcp: jsonMCP(filepath.Join(h, ".fx", "mcp.json"), "mcpServers", func(exe string) map[string]any {
				return map[string]any{"type": "stdio", "command": []string{exe, "mcp"}}
			}),
			rules: []string{filepath.Join(h, ".fx", "AGENTS.md")}},
		{name: "Kiro", dirs: []string{filepath.Join(h, ".kiro")},
			mcp:   jsonMCP(filepath.Join(h, ".kiro", "settings", "mcp.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".kiro", "steering", "AGENTS.md")}},
		{name: "Windsurf / Devin", dirs: []string{filepath.Join(h, ".codeium", "windsurf")},
			mcp:   jsonMCP(filepath.Join(h, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".codeium", "windsurf", "memories", "global_rules.md")}},
		{name: "Devin CLI", dirs: []string{filepath.Join(h, ".devin")},
			rules: []string{filepath.Join(h, ".devin", "global_rules.md")}},
		{name: "Copilot CLI", dirs: []string{filepath.Join(h, ".copilot")},
			mcp: jsonMCP(filepath.Join(h, ".copilot", "mcp-config.json"), "mcpServers", func(exe string) map[string]any {
				return map[string]any{"type": "local", "command": exe, "args": []string{"mcp"}, "tools": []string{"*"}}
			}),
			rules: []string{filepath.Join(h, ".copilot", "copilot-instructions.md")}},
		{name: "Amp", dirs: []string{filepath.Join(cfgHome(), "amp")},
			mcp:   jsonMCP(filepath.Join(cfgHome(), "amp", "settings.json"), "amp.mcpServers", stdio),
			rules: []string{filepath.Join(cfgHome(), "amp", "AGENTS.md")}},
		{name: "Qwen Code", dirs: []string{filepath.Join(h, ".qwen")},
			mcp:   jsonMCP(filepath.Join(h, ".qwen", "settings.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".qwen", "QWEN.md")}},
		{name: "Crush", dirs: []string{filepath.Join(cfgHome(), "crush")},
			rules: []string{filepath.Join(cfgHome(), "crush", "CRUSH.md")}},
		{name: "Cline", dirs: []string{filepath.Join(h, ".cline")},
			mcp:   jsonMCP(filepath.Join(h, ".cline", "data", "settings", "cline_mcp_settings.json"), "mcpServers", stdio),
			rules: []string{filepath.Join(h, ".cline", "rules", "firefighter.md")}},
		{name: "Goose", dirs: []string{filepath.Join(appData(), "Block", "goose"), filepath.Join(cfgHome(), "goose")},
			rules: []string{filepath.Join(h, ".agents", "AGENTS.md")}}, // also read by Kimi Code, Cline, Droid
		{name: "Droid", dirs: []string{filepath.Join(h, ".factory")},
			rules: []string{filepath.Join(h, ".agents", "AGENTS.md")}},
	}
}

func (a agent) installed() bool {
	for _, d := range a.dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}
