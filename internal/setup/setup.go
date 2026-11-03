// Package setup makes firefighter set-and-forget: it starts the guard at logon, registers the MCP
// server with every agent CLI it finds, and adds the shared-machine rules to each agent's global
// instructions so every new session (Claude Code, Codex, Gemini, Cursor, and the threads of apps
// built on them) knows to queue heavy work and say how long it will be busy.
package setup

import (
	"regexp"

	"github.com/BurntSushi/toml"

	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const begin, end = "<!-- firefighter:begin -->", "<!-- firefighter:end -->"

// Instructions is the block added to agents' global instruction files.
const Instructions = begin + `
## Shared machine: firefighter
This PC's CPU, GPU and RAM are shared by several AI agents and a human who always comes first.
- Run every GPU job, and any CPU job over ~1 min, through firefighter as a background task:
  ` + "`firefighter run [--gpu] --minutes N [--vram GB] --name \"<project> <what>\" -- <cmd>`" + `
  It waits its turn, then runs here (venv/conda/docker work) and prints a receipt.
- ` + "`--minutes`" + ` is a lease: give your high-end guess. Past 1.25x it is billed 2x and preempted first unless you run ` + "`firefighter extend <id> <min>`" + `.
- Every reply ends with ` + "`do: ...`" + `. Follow it. Add ` + "`--urgent`" + ` only when the human is waiting on the result (2x price); ` + "`--spot`" + ` for work that can wait (1/3 price, paused first).
- Tell the human the start time it shows. Don't run heavy work outside it or change its config: outside work is found, billed 1.5x and preempted first.
- Heavy work later: ` + "`firefighter plan \"<what>\" --when HH:MM --minutes N --gpu 90`" + `. Long GPU loops: ` + "`firefighter protocol`" + `.
` + end + "\n"

type Step struct {
	What string
	Err  error
}

// Install does everything. exe is the absolute path of the firefighter binary.
func Install(exe string) []Step {
	var steps []Step
	add := func(what string, err error) { steps = append(steps, Step{what, err}) }
	if os.Getenv("FIREFIGHTER_NO_AUTOSTART") == "" {
		add("start the guard at logon", autostart(exe))
	}
	for _, f := range instructionFiles() {
		add("agent rules -> "+f, upsertBlock(f, Instructions))
	}
	for _, r := range registerMCP(exe) {
		steps = append(steps, r)
	}
	for _, a := range agents() {
		if a.installed() && a.mcp != nil && os.Getenv("FIREFIGHTER_NO_CLI_REGISTER") == "" {
			add("MCP -> "+a.name, a.mcp(exe, true))
		}
	}
	return steps
}

func Uninstall() []Step {
	var steps []Step
	add := func(what string, err error) { steps = append(steps, Step{what, err}) }
	add("remove logon start", removeAutostart())
	for _, f := range allInstructionFiles() {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		add("remove agent rules from "+f, upsertBlock(f, ""))
		if filepath.Base(f) == "firefighter.md" { // a rules file of its own: gone entirely
			if b, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(b)) == "" {
				_ = os.Remove(f)
			}
		}
	}
	steps = append(steps, unregisterMCP()...)
	for _, a := range agents() {
		if a.mcp != nil {
			if err := a.mcp("", false); err != nil {
				add("MCP <- "+a.name, err)
			}
		}
	}
	return steps
}

func home() string { h, _ := os.UserHomeDir(); return h }

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(), ".claude")
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".codex")
}

// instructionFiles are the global instruction files of agent CLIs that are installed.
func instructionFiles() []string {
	var out []string
	cands := []struct{ dir, file string }{
		{claudeDir(), "CLAUDE.md"},
		{codexDir(), "AGENTS.md"},
		{filepath.Join(home(), ".gemini"), "GEMINI.md"},
	}
	for _, c := range cands {
		if fi, err := os.Stat(c.dir); err == nil && fi.IsDir() {
			out = append(out, filepath.Join(c.dir, c.file))
		}
	}
	seen := map[string]bool{}
	for _, f := range out {
		seen[f] = true
	}
	for _, a := range agents() {
		for _, f := range a.rules {
			if a.installed() && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	if len(out) == 0 { // no agent CLI yet: still teach Claude Code, the most common
		out = append(out, filepath.Join(claudeDir(), "CLAUDE.md"))
	}
	return out
}

// allInstructionFiles is every file setup may have written, installed or not (for uninstall).
func allInstructionFiles() []string {
	out := []string{filepath.Join(claudeDir(), "CLAUDE.md"), filepath.Join(codexDir(), "AGENTS.md"), filepath.Join(home(), ".gemini", "GEMINI.md")}
	seen := map[string]bool{}
	for _, f := range out {
		seen[f] = true
	}
	for _, a := range agents() {
		for _, f := range a.rules {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// upsertBlock replaces the firefighter block in a file (or appends it); block "" removes it.
func upsertBlock(path, block string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(b)
	nb, ne := strings.Count(s, begin), strings.Count(s, end)
	if nb > 1 || ne > 1 || nb != ne {
		return fmt.Errorf("%s has %d begin and %d end firefighter markers; fix them by hand (left untouched)", path, nb, ne)
	}
	if i := strings.Index(s, begin); i >= 0 {
		j := strings.Index(s, end)
		if j < i {
			return fmt.Errorf("%s has the firefighter end marker before the begin marker (left untouched)", path)
		}
		s = s[:i] + strings.TrimPrefix(s[j+len(end):], "\n")
	}
	if block != "" {
		if s != "" && !strings.HasSuffix(s, "\n\n") {
			s = strings.TrimRight(s, "\n") + "\n\n"
		}
		s += block
	} else if s == "" && os.IsNotExist(err) {
		return nil
	}
	return writeFile(path, []byte(s))
}

// writeFile replaces a config file safely: a one-time .firefighter.bak of the original, then a
// temp file renamed over it, so a crash never leaves it truncated.
func writeFile(path string, b []byte) error {
	// A symlinked config (stow, chezmoi, a dotfiles repo) is edited where it lives: renaming over
	// the link would cut it off from its repo.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		bak := path + ".firefighter.bak"
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			if old, err := os.ReadFile(path); err == nil {
				_ = os.WriteFile(bak, old, mode)
			}
		}
	}
	tmp := path + ".firefighter.tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- MCP registration ---------------------------------------------------------------------------

func registerMCP(exe string) []Step {
	var steps []Step
	if claude, err := exec.LookPath("claude"); err == nil && os.Getenv("FIREFIGHTER_NO_CLI_REGISTER") == "" {
		_ = exec.Command(claude, "mcp", "remove", "--scope", "user", "firefighter").Run()
		out, err := exec.Command(claude, "mcp", "add", "--scope", "user", "firefighter", "--", exe, "mcp").CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		steps = append(steps, Step{"MCP -> Claude Code", err})
	} else if _, err := os.Stat(claudeDir()); err == nil {
		steps = append(steps, Step{"MCP -> Claude Code (~/.claude.json)", jsonServers(claudeJSON(), "mcpServers", exe, true)})
	}
	if _, err := os.Stat(codexDir()); err == nil {
		steps = append(steps, Step{"MCP -> Codex", codexTOML(exe, true)})
	}
	if _, err := os.Stat(filepath.Join(home(), ".gemini")); err == nil {
		steps = append(steps, Step{"MCP -> Gemini CLI", jsonServers(filepath.Join(home(), ".gemini", "settings.json"), "mcpServers", exe, true)})
	}
	if _, err := os.Stat(filepath.Join(home(), ".cursor")); err == nil {
		steps = append(steps, Step{"MCP -> Cursor", jsonServers(filepath.Join(home(), ".cursor", "mcp.json"), "mcpServers", exe, true)})
	}
	return steps
}

func unregisterMCP() []Step {
	var steps []Step
	if claude, err := exec.LookPath("claude"); err == nil && os.Getenv("FIREFIGHTER_NO_CLI_REGISTER") == "" {
		steps = append(steps, Step{"MCP <- Claude Code", exec.Command(claude, "mcp", "remove", "--scope", "user", "firefighter").Run()})
	}
	steps = append(steps, Step{"MCP <- ~/.claude.json", jsonServers(claudeJSON(), "mcpServers", "", false)})
	steps = append(steps, Step{"MCP <- Codex", codexTOML("", false)})
	steps = append(steps, Step{"MCP <- Gemini CLI", jsonServers(filepath.Join(home(), ".gemini", "settings.json"), "mcpServers", "", false)})
	steps = append(steps, Step{"MCP <- Cursor", jsonServers(filepath.Join(home(), ".cursor", "mcp.json"), "mcpServers", "", false)})
	return steps
}

func claudeJSON() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home(), ".claude.json")
}

// jsonServers adds or removes {"firefighter": {"type": "stdio", "command": exe, "args": ["mcp"]}}
// under key.
func jsonServers(path, key, exe string, add bool) error {
	var e map[string]any
	if add {
		e = map[string]any{"type": "stdio", "command": exe, "args": []string{"mcp"}}
	}
	return jsonEntry(path, key, e, add, nil)
}

// jsonEntry adds (entry) or removes the "firefighter" server under key in a JSON config. A new or
// empty file starts from fresh (fields some agents require, like Muse's schema_version).
func jsonEntry(path, key string, entry map[string]any, add bool, fresh map[string]any) error {
	m := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) && !add {
		return nil
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s isn't valid JSON (comments?), left alone: %w", path, err)
		}
	} else if add {
		for k, v := range fresh {
			m[k] = v
		}
	}
	servers, _ := m[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if add {
		servers["firefighter"] = entry
	} else {
		if _, ok := servers["firefighter"]; !ok {
			return nil
		}
		delete(servers, "firefighter")
	}
	m[key] = servers
	out, _ := json.MarshalIndent(m, "", "  ")
	return writeFile(path, out)
}

// The firefighter table and its subtables ([mcp_servers.firefighter.env], ...).
var codexHeader = regexp.MustCompile(`^[ \t]*\[[ \t]*mcp_servers[ \t]*\.[ \t]*"?firefighter"?[ \t]*(\.[^\]]*)?\][ \t]*(#.*)?$`)
var anyHeader = regexp.MustCompile(`^[ \t]*\[`)

// codexTOML adds or removes the [mcp_servers.firefighter] table in Codex's config.toml. It edits
// whole lines only, then parses the result and refuses to write anything that isn't valid TOML
// with exactly the intended change.
func codexTOML(exe string, add bool) error {
	return tomlServers(filepath.Join(codexDir(), "config.toml"), exe, add)
}

// tomlServers edits [mcp_servers.firefighter] in a Codex-style config.toml (Codex, Grok Build).
func tomlServers(path, exe string, add bool) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) && !add {
		return nil
	}
	var out []string
	skipping := false
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		switch {
		case codexHeader.MatchString(line):
			skipping = true
			continue
		case skipping && anyHeader.MatchString(line) && !codexHeader.MatchString(line):
			skipping = false
		}
		if !skipping {
			out = append(out, line)
		}
	}
	s := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if add {
		if s != "" {
			s += "\n\n"
		}
		s += fmt.Sprintf("[mcp_servers.firefighter]\ncommand = %q\nargs = [\"mcp\"]", exe)
	}
	s += "\n"
	var check map[string]any
	if _, err := toml.Decode(s, &check); err != nil {
		return fmt.Errorf("%s: the edited file wouldn't parse, left untouched: %w", path, err)
	}
	servers, _ := check["mcp_servers"].(map[string]any)
	if _, has := servers["firefighter"]; has != add {
		return fmt.Errorf("%s: couldn't edit the firefighter entry cleanly, left untouched", path)
	}
	return writeFile(path, []byte(s))
}

// ---- start at logon -----------------------------------------------------------------------------

func autostart(exe string) error {
	switch runtime.GOOS {
	case "windows":
		// conhost --headless runs the console program with no window at all.
		// conhost --headless runs the console program with no window at all.
		tr := fmt.Sprintf(`conhost.exe --headless "%s" daemon`, exe)
		user := os.Getenv("USERNAME")
		if dom := os.Getenv("USERDOMAIN"); dom != "" && user != "" {
			user = dom + `\` + user
		}
		args := []string{"/Create", "/F", "/TN", "firefighter", "/SC", "ONLOGON", "/RL", "LIMITED", "/TR", tr}
		if user != "" {
			args = append(args, "/RU", user, "/IT")
		}
		out, err := exec.Command("schtasks", args...).CombinedOutput()
		if err == nil {
			return nil
		}
		// Without rights to create logon tasks: the per-user Run key needs no admin.
		if rerr := exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "firefighter", "/t", "REG_SZ", "/d", tr, "/f").Run(); rerr == nil {
			return nil
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	case "darwin":
		dir := filepath.Join(home(), "Library", "LaunchAgents")
		path := filepath.Join(dir, "dev.firefighter.plist")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.firefighter</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ProcessType</key><string>Interactive</string>
</dict></plist>
`, exe)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			return err
		}
		uid := fmt.Sprint(os.Getuid())
		_ = exec.Command("launchctl", "bootout", "gui/"+uid, path).Run()
		return exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run()
	default:
		if exec.Command("systemctl", "--user", "show-environment").Run() == nil {
			dir := filepath.Join(home(), ".config", "systemd", "user")
			unit := fmt.Sprintf("[Unit]\nDescription=firefighter: shares this machine between AI agents, the human first\n\n[Service]\nExecStart=%q daemon\nExecStop=%q daemon --stop\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n", exe, exe)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "firefighter.service"), []byte(unit), 0o644); err != nil {
				return err
			}
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			if err := exec.Command("systemctl", "--user", "enable", "firefighter.service").Run(); err != nil {
				return err
			}
			return exec.Command("systemctl", "--user", "restart", "firefighter.service").Run() // picks up a new binary
		}
		// No systemd user session (containers, WSL without systemd): XDG autostart; the CLI also
		// starts the daemon on demand.
		dir := filepath.Join(home(), ".config", "autostart")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "firefighter.desktop"),
			[]byte(fmt.Sprintf("[Desktop Entry]\nType=Application\nName=firefighter\nExec=%s daemon\nX-GNOME-Autostart-enabled=true\n", exe)), 0o644)
	}
}

func removeAutostart() error {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("schtasks", "/Delete", "/F", "/TN", "firefighter").Run()
		_ = exec.Command("reg", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "firefighter", "/f").Run()
	case "darwin":
		path := filepath.Join(home(), "Library", "LaunchAgents", "dev.firefighter.plist")
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d", os.Getuid()), path).Run()
		_ = os.Remove(path)
	default:
		_ = exec.Command("systemctl", "--user", "disable", "--now", "firefighter.service").Run()
		_ = os.Remove(filepath.Join(home(), ".config", "systemd", "user", "firefighter.service"))
		_ = os.Remove(filepath.Join(home(), ".config", "autostart", "firefighter.desktop"))
	}
	return nil
}
