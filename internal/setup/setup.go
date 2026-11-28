// Package setup configures logon startup on macOS and Linux, registers the MCP
// server with every agent CLI it finds, and adds the shared-machine rules to each agent's global
// instructions so every new session (Claude Code, Codex, Gemini, Cursor, and the threads of apps
// built on them) knows to queue heavy work and say how long it will be busy.
package setup

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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
- Every ` + "`docker run`" + `, even a 10-second look inside an image, goes through it too: ` + "`firefighter run --quick -- docker run --rm <image> ls /x`" + ` (a short job, so it can start ahead of longer ones; the file cache it read is given back after). A big image read outside it fills RAM for minutes.
- A waiting job's ` + "`--ram`/`--vram`/`--minutes`" + ` off? ` + "`firefighter amend <id> --vram GB`" + ` keeps its place; don't cancel and resubmit.
- Big files (datasets, checkpoints, models, caches) go in your project's storage space, not the human's folders: ` + "`firefighter storage claim --gb N --why \"<what>\"`" + ` prints its folder. Rank everything you save: ` + "`firefighter storage label <path> --rank keep|useful|scratch --for \"<what uses it, until when>\"`" + `. Stay under your claim: ` + "`firefighter storage free --gb N`" + ` deletes the least important first (scratch, then with --upto unranked/useful; never keep); ` + "`firefighter storage items`" + ` lists yours, ` + "`firefighter storage release`" + ` when done.
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
	if !installerManagesAutostart && os.Getenv("FIREFIGHTER_NO_AUTOSTART") == "" {
		add("start the guard at logon", autostart(exe))
	}
	for _, f := range instructionFiles() {
		add("agent rules -> "+f, upsertBlock(f, Instructions))
	}
	steps = append(steps, registerMCP(exe)...)
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
	if os.Getenv("FIREFIGHTER_NO_AUTOSTART") == "" {
		add("remove logon start", removeAutostart())
	}
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

// writeFile replaces a config file safely: a one-time .firefighter.bak of the original, a
// timestamped backup before every write (the newest 3 kept), then a temp file renamed over it, so
// a crash never leaves it truncated.
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
		backupBeforeWrite(path, mode)
		bak := path + ".firefighter.bak"
		if _, err := os.Lstat(bak); os.IsNotExist(err) {
			if old, err := os.ReadFile(path); err == nil {
				// Exclusive create: never through a link someone planted at the backup's name.
				if f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode); err == nil {
					_, _ = f.Write(old)
					_ = f.Close()
				}
			}
		}
	}
	tmp := path + ".firefighter.tmp"
	_ = os.Remove(tmp) // a planted link is removed, never written through
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// backupBeforeWrite copies path to path.firefighter.bak-<time> and keeps the newest 3 of those.
func backupBeforeWrite(path string, mode os.FileMode) {
	old, err := os.ReadFile(path)
	if err != nil {
		return
	}
	bak := path + ".firefighter.bak-" + time.Now().Format("20060102-150405.000")
	if f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode); err == nil { // never through a planted link
		_, _ = f.Write(old)
		_ = f.Close()
	}
	matches, _ := filepath.Glob(path + ".firefighter.bak-*")
	sort.Strings(matches) // the time stamp sorts oldest first
	for len(matches) > 3 {
		_ = os.Remove(matches[0])
		matches = matches[1:]
	}
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
// empty file starts from fresh (fields some agents require, like Muse's schema_version). Only that
// entry changes: every other key keeps its place and value (security audit M3), nothing is written
// when nothing changes, and a file another program rewrote while this one worked is read again.
func jsonEntry(path, key string, entry map[string]any, add bool, fresh map[string]any) error {
	for try := 0; ; try++ {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if os.IsNotExist(err) && !add {
			return nil
		}
		root := newOmap()
		if len(strings.TrimSpace(string(b))) > 0 {
			v, err := parseOrdered(b)
			m, isObj := v.(*omap)
			if err == nil && !isObj {
				err = errors.New("not a JSON object")
			}
			if err != nil {
				return fmt.Errorf("%s isn't valid JSON (comments?), left alone: %w", path, err)
			}
			root = m
		} else if add && fresh != nil {
			root, _ = toOrdered(fresh).(*omap)
		}
		v, _ := root.get(key)
		servers, _ := v.(*omap)
		if servers == nil {
			servers = newOmap()
		}
		if add {
			servers.set("firefighter", toOrdered(entry))
		} else {
			if _, ok := servers.get("firefighter"); !ok {
				return nil
			}
			servers.del("firefighter")
		}
		root.set(key, servers)
		out := marshalOrdered(root)
		if len(b) > 0 {
			if same, err := parseOrdered(b); err == nil && bytes.Equal(marshalOrdered(same), out) {
				return nil // already as wanted
			}
		}
		// Another program (a running Claude Code saving ~/.claude.json) may have written it since it
		// was read: start over from its new content rather than overwrite that.
		if now, _ := os.ReadFile(path); !bytes.Equal(now, b) && try < 3 {
			continue
		}
		return writeFile(path, out)
	}
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
	// Everything else must be exactly as it was: a line that only looks like a table header (inside
	// a multiline string, say) must not have cut the human's settings (review 11/20).
	var before map[string]any
	if _, err := toml.Decode(string(b), &before); err == nil {
		if !reflect.DeepEqual(withoutFirefighter(before), withoutFirefighter(check)) {
			return fmt.Errorf("%s: editing the firefighter entry would change other settings, left untouched", path)
		}
	}
	return writeFile(path, []byte(s))
}

// withoutFirefighter is a decoded config.toml minus mcp_servers.firefighter (and an mcp_servers
// table left empty by that), for comparing the rest.
func withoutFirefighter(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	if servers, ok := out["mcp_servers"].(map[string]any); ok {
		rest := map[string]any{}
		for k, v := range servers {
			if k != "firefighter" {
				rest[k] = v
			}
		}
		if len(rest) == 0 {
			delete(out, "mcp_servers")
		} else {
			out["mcp_servers"] = rest
		}
	}
	return out
}

// ---- start at logon -----------------------------------------------------------------------------

func autostart(exe string) error {
	switch runtime.GOOS {
	case "darwin":
		dir := filepath.Join(home(), "Library", "LaunchAgents")
		path := filepath.Join(dir, "dev.firefighter.plist")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.firefighter</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ProcessType</key><string>Interactive</string>
</dict></plist>
`, xmlEscape(exe))
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
			[]byte(fmt.Sprintf("[Desktop Entry]\nType=Application\nName=firefighter\nExec=%s daemon\nX-GNOME-Autostart-enabled=true\n", desktopQuote(exe))), 0o644)
	}
}

func removeAutostart() error {
	switch runtime.GOOS {
	case "windows":
		return removeStartupShortcut()
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

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// desktopQuote quotes a path for a .desktop Exec= line (spaces and reserved characters).
func desktopQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}

// RuleState is one agent instruction file's firefighter block: "current", "outdated" (an older
// firefighter wrote it: `firefighter setup` refreshes it), or "missing".
type RuleState struct {
	Path, State string
}

// Rules reports the firefighter block in every installed agent's global instruction file.
func Rules() []RuleState {
	var out []RuleState
	for _, f := range instructionFiles() {
		b, err := os.ReadFile(f)
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		switch {
		case err != nil || !strings.Contains(s, begin):
			out = append(out, RuleState{f, "missing"})
		case strings.Contains(s, strings.TrimSuffix(Instructions, "\n")):
			out = append(out, RuleState{f, "current"})
		default:
			out = append(out, RuleState{f, "outdated"})
		}
	}
	return out
}
