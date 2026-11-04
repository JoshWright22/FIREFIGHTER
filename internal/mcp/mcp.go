// Package mcp serves firefighter to agents over the Model Context Protocol (stdio, JSON-RPC 2.0).
// Replies are one dense line of text (what an agent reads) plus structuredContent (for programs),
// so a waiting agent spends tens of tokens per call, not hundreds.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/daemon"
)

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// listed is a tool as tools/list shows it, with MCP annotations.
type listed struct {
	tool
	Annotations map[string]any `json:"annotations,omitempty"`
}

// submit runs a shell command as the user, outside the calling agent's own sandbox: say so, and
// let hosts ask for approval as they would for a shell tool. The rest only read or change queue
// state.
func annotated(ts []tool) []listed {
	out := make([]listed, len(ts))
	for i, t := range ts {
		out[i] = listed{tool: t}
		switch t.Name {
		case "firefighter_submit":
			out[i].Description += " Runs the command as you, outside any agent sandbox, with your network and files: only submit what you'd run in your own shell."
			out[i].Annotations = map[string]any{"title": "Run a command through the queue", "destructiveHint": true, "openWorldHint": true, "readOnlyHint": false}
		case "firefighter_status", "firefighter_wait":
			out[i].Annotations = map[string]any{"readOnlyHint": true, "openWorldHint": false}
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func num(d string) map[string]any { return map[string]any{"type": "number", "description": d} }
func boo(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }

var baseTools = []tool{
	{"firefighter_submit", "Queue heavy work (GPU, or CPU over ~1 min) on this shared machine. Returns one line: job id, start estimate, cost, do: (next step). Then call firefighter_wait.", obj(map[string]any{
		"command":    str("Shell command"),
		"cwd":        str("Absolute working directory"),
		"minutes":    num("High-end runtime guess; a lease, billed 2x past 1.25x"),
		"gpu":        boo("Needs the GPU"),
		"vram_gb":    num("Peak VRAM GB, if known"),
		"ram_gb":     num("Peak RAM GB, if known"),
		"urgent":     boo("Only when the human waits on the result; 2x price"),
		"spot":       boo("Leftover time only; 1/3 price, paused first"),
		"net":        boo("Network-heavy (downloads, uploads); known installers and downloads are detected"),
		"gpu_vendor": str("GPUs the job can use: nvidia, amd, intel or any (default NVIDIA if present)"),
		"name":       str("'<project> <what>'"),
	}, "command", "cwd", "minutes")},
	{"firefighter_wait", "Wait up to ~45 s on a job. One line, short if nothing changed; receipt and log tail when done.", obj(map[string]any{
		"id": str("Job id"), "timeout_s": num("Seconds, max 50"),
	}, "id")},
	{"firefighter_status", "The shared machine in a few lines: the human, what runs until when, the queue, budgets, plans, do:. Pass minutes (and gpu) to also price a job without queuing it.", obj(map[string]any{
		"minutes": num("Price a job of this runtime"), "gpu": boo("...on the GPU"), "vram_gb": num("...needing this VRAM"), "project": str("Your project (folder name)"),
	})},
	{"firefighter_job", "Change your job: extend (minutes = more lease), urgent / normal (tier, while waiting), cancel.", obj(map[string]any{
		"id": str("Job id"), "action": str("extend | urgent | normal | cancel"), "minutes": num("For extend"),
	}, "id", "action")},
	{"firefighter_plan", "Announce heavy work later so others plan around it (reserves nothing). done=true removes your project's plans.", obj(map[string]any{
		"what": str("What"), "at": str("HH:MM or now"), "minutes": num("Minutes"), "gpu_pct": num("GPU %"), "vram_gb": num("VRAM GB"), "project": str("Project"), "done": boo("Remove"),
	})},
}

var configTool = tool{"firefighter_config", "Change firefighter's policy (the human allowed agents to). action: show | set | unset. Keys like order.policy, pricing.policy, gpu.max_jobs.", obj(map[string]any{
	"action": str("show | set | unset"), "key": str("Dotted key"), "value": str("New value"),
}, "action")}

func tools() []tool {
	c, _ := config.Load()
	if c.Guard.AgentsCanConfigure {
		return append(append([]tool{}, baseTools...), configTool)
	}
	return baseTools
}

// Serve runs the MCP server on stdin/stdout until stdin closes. Calls run concurrently, so a long
// wait never blocks a ping or another tool call.
func Serve(version string) error {
	in := bufio.NewReader(os.Stdin)
	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = out.Encode(v)
	}
	var wg sync.WaitGroup
	for {
		line, err := readLine(in, 8<<20)
		if len(strings.TrimSpace(string(line))) > 0 {
			var req rpc
			if json.Unmarshal(line, &req) != nil {
				send(rpc{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			} else if req.ID != nil && string(req.ID) != "null" {
				wg.Add(1)
				go func() { defer wg.Done(); send(handle(req, version)) }()
			}
		}
		if err == io.EOF {
			wg.Wait()
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func handle(req rpc, version string) rpc {
	resp := rpc{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "firefighter", "version": version},
			"instructions":    "Shared machine; the human comes first. Queue heavy work (GPU, or CPU over ~1 min) with firefighter_submit, then firefighter_wait. Every reply ends with do: follow it.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": annotated(tools())}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{-32602, err.Error()}
			return resp
		}
		text, data, err := call(p.Name, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}
		} else {
			r := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
			if data != nil {
				r["structuredContent"] = data
			}
			resp.Result = r
		}
	default:
		resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
	}
	return resp
}

func s(a map[string]any, k string) string {
	v, _ := a[k].(string)
	return v
}

func f(a map[string]any, k string) float64 {
	switch v := a[k].(type) {
	case float64:
		return v
	case string:
		var x float64
		fmt.Sscan(v, &x)
		return x
	}
	return 0
}

func b(a map[string]any, k string) bool { v, _ := a[k].(bool); return v }

// jobLine is a job in one line: what an agent needs to decide, nothing else.
func jobLine(v daemon.View) string {
	switch v.State {
	case "done":
		r := v.Receipt
		if r == nil {
			return v.ID + " done"
		}
		next := ""
		if r.Requeue {
			why := r.Reason
			if why == "" {
				why = "the human needed the machine"
			}
			next = " | stopped because " + why + "; requeued automatically (same id), it restarts when there's room | do: call firefighter_wait again; nothing to resubmit"
		}
		return fmt.Sprintf("%s done %s exit %d | ran %.0fm (declared %.0f) waited %.0fm | charged %.0f | peak %.1fGB VRAM %.1fGB RAM%s",
			v.ID, r.Outcome, r.Exit, r.Minutes, r.Declared, r.Waited, r.Charged, r.PeakVRAM, r.PeakRAM, next)
	case "waiting":
		l := fmt.Sprintf("%s waiting #%d (%s), starts ~%s: %s | budget %.0f", v.ID, v.Position, v.Tier, v.ETA, v.Why, v.Budget)
		if v.Quote != nil {
			l += " | do: " + v.Quote.Do
		}
		return l
	case "running":
		note := ""
		if v.Paused {
			note = " (paused: " + v.Why + ")"
		}
		if v.Checkpt {
			note = " (asked to checkpoint and exit: " + v.Why + ")"
		}
		return fmt.Sprintf("%s running, ends ~%s%s | do: wait; extend with firefighter_job if it will run long", v.ID, v.ETA, note)
	case "granted":
		return fmt.Sprintf("%s starting now, ends ~%s | do: call firefighter_wait", v.ID, v.ETA)
	}
	return v.ID + " " + v.State
}

func call(name string, a map[string]any) (string, any, error) {
	if name == "firefighter_config" {
		t, err := configCall(a)
		return t, nil, err
	}
	c, err := client.Connect()
	if err != nil {
		return "", nil, err
	}
	switch name {
	case "firefighter_status":
		st, err := c.Status()
		if err != nil {
			return "", nil, err
		}
		text := statusText(st)
		if f(a, "minutes") > 0 {
			q, err := c.Quote(b(a, "gpu"), false, f(a, "minutes"), f(a, "vram_gb"), 0, s(a, "project"), "")
			if err == nil {
				text += "\nquote: " + q.Line()
			}
		}
		return text, st, nil
	case "firefighter_submit":
		if s(a, "command") == "" || f(a, "minutes") <= 0 {
			return "", nil, fmt.Errorf("command and minutes are required")
		}
		if fi, err := os.Stat(s(a, "cwd")); err != nil || !fi.IsDir() {
			return "", nil, fmt.Errorf("cwd %q isn't a folder", s(a, "cwd"))
		}
		args := []string{"--minutes", fmt.Sprint(f(a, "minutes"))}
		for _, k := range []string{"gpu", "spot", "urgent", "net"} {
			if b(a, k) {
				args = append(args, "--"+k)
			}
		}
		for _, k := range []string{"vram_gb", "ram_gb"} {
			if v := f(a, k); v > 0 {
				args = append(args, "--"+strings.TrimSuffix(k, "_gb"), fmt.Sprint(v))
			}
		}
		if v := s(a, "name"); v != "" {
			args = append(args, "--name", v)
		}
		if v := s(a, "gpu_vendor"); v != "" {
			args = append(args, "--gpu-vendor", v)
		}
		shell := []string{"sh", "-c"}
		if runtime.GOOS == "windows" {
			shell = []string{"cmd", "/c"}
		}
		args = append(append(append(args, "--"), shell...), s(a, "command"))
		id, _, err := client.Detached(args, s(a, "cwd"))
		if err != nil {
			return "", nil, err
		}
		v, err := c.GetSoon(id)
		if err != nil {
			return "", nil, fmt.Errorf("%v\n%s", err, client.Tail(client.LogPath(id), 800))
		}
		if v.State == "waiting" || v.State == "granted" {
			return "queued " + jobLine(v), v, nil
		}
		return "started " + jobLine(v), v, nil
	case "firefighter_wait":
		t := f(a, "timeout_s")
		if t <= 0 || t > 50 {
			t = 45 // under common 60 s tool timeouts
		}
		deadline := time.Now().Add(time.Duration(t * float64(time.Second)))
		id := s(a, "id")
		if !jobID(id) {
			return "", nil, fmt.Errorf("id must be a firefighter job id (8 hex characters)")
		}
		v, err := c.GetSoon(id)
		first := v
		var requeuedAt time.Time
		for err == nil && (v.State != "done" || (v.Receipt != nil && v.Receipt.Requeue)) {
			left := int(time.Until(deadline).Seconds())
			if left < 1 {
				break
			}
			if v.State == "done" { // evicted and requeued: the next attempt registers shortly
				if requeuedAt.IsZero() {
					requeuedAt = time.Now()
				} else if time.Since(requeuedAt) > 15*time.Second {
					// Nobody re-registered: the client that would requeue it is gone.
					return jobLine(v) + "\nrequeued, but no new attempt registered (its client exited); submit it again", v, nil
				}
				time.Sleep(500 * time.Millisecond)
				v, err = c.GetSoon(id)
				continue
			}
			v, err = c.Get(id, v.State, min(20, left))
		}
		if err != nil {
			return "", nil, fmt.Errorf("%v\n%s", err, client.Tail(client.LogPath(id), 800))
		}
		if v.State == "done" {
			return jobLine(v) + "\nlog tail:\n" + client.Tail(client.LogPath(id), 1000), v, nil
		}
		if v.State == first.State && v.Position == first.Position && v.ETA == first.ETA {
			return fmt.Sprintf("%s unchanged: %s #%d, ~%s | do: call firefighter_wait again", v.ID, v.State, v.Position, v.ETA), v, nil
		}
		return jobLine(v), v, nil
	case "firefighter_job":
		id := s(a, "id")
		if !jobID(id) {
			return "", nil, fmt.Errorf("id must be a firefighter job id (8 hex characters)")
		}
		var v daemon.View
		me := callerProject()
		switch s(a, "action") {
		case "extend":
			v, err = c.Act(id, "extend", daemon.Act{Minutes: f(a, "minutes"), Project: me})
		case "urgent":
			v, err = c.Act(id, "bid", daemon.Act{Minutes: 10, Project: me})
		case "normal":
			v, err = c.Act(id, "bid", daemon.Act{Minutes: 5, Project: me})
		case "cancel":
			v, err = c.Act(id, "cancel", daemon.Act{Project: me})
		default:
			return "", nil, fmt.Errorf("action must be extend, urgent, normal or cancel")
		}
		if err != nil {
			return "", nil, err
		}
		return jobLine(v), v, nil
	case "firefighter_plan":
		project := s(a, "project")
		if project == "" {
			project = callerProject() // the same default for announcing and for removing
		}
		if b(a, "done") {
			return "plans removed", nil, c.Unplan(project)
		}
		at := s(a, "at")
		if at == "" {
			at = s(a, "when")
		}
		p, err := c.Plan(daemon.Plan{Project: project, What: s(a, "what"), When: at, Minutes: f(a, "minutes"),
			GPUPct: f(a, "gpu_pct"), VRAMGB: f(a, "vram_gb"), RAMGB: f(a, "ram_gb")})
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("announced %s: %s at %s for %.0fm", p.ID, p.What, p.When, p.Minutes), p, nil
	}
	return "", nil, fmt.Errorf("unknown tool %s", name)
}

// statusText is the machine in a few lines (about 100 tokens plus ~12 per job).
func statusText(st daemon.Status) string {
	var b strings.Builder
	m := st.Machine
	who := st.User
	if st.UserWhy != "" {
		who += " (" + st.UserWhy + ")"
	}
	fmt.Fprintf(&b, "human: %s | reserve %.0f CPU %.0f%% RAM %.1fGB VRAM | free: RAM %.1f/%.0fGB, VRAM %.1f/%.0fGB, GPU %.0f%% busy\n",
		who, st.Reserve["cpus"], st.Reserve["ram_pct"], st.Reserve["vram_gb"], m["ram_avail_gb"], m["ram_total_gb"],
		m["vram_total_gb"]-m["vram_used_gb"], m["vram_total_gb"], m["gpu_util"])
	for _, v := range st.Running {
		fmt.Fprintf(&b, "running %s %q (%s) until ~%s\n", v.ID, v.Name, v.Project, v.ETA)
	}
	for _, v := range st.Waiting {
		fmt.Fprintf(&b, "waiting #%d %s %q (%s) starts ~%s: %s\n", v.Position, v.ID, v.Name, v.Project, v.ETA, v.Why)
	}
	for _, p := range st.Plans {
		fmt.Fprintf(&b, "plan %s: %s at %s, %.0fm\n", p.Project, p.What, p.When, p.Minutes)
	}
	if q, ok := st.Market["gpu"]; ok {
		b.WriteString("market: " + q.Line())
	}
	return strings.TrimSpace(b.String())
}

func configCall(a map[string]any) (string, error) {
	act := s(a, "action")
	if act == "set" || act == "unset" {
		c, _ := config.Load()
		if !c.Guard.AgentsCanConfigure {
			return "", fmt.Errorf("only the human changes firefighter's policy")
		}
		if !agentKey(s(a, "key")) {
			return "", fmt.Errorf("agents may only change scheduling policy (order.*, pricing.*, gpu.*); the rest, presets included, is the human's")
		}
	}
	switch act {
	case "set":
		if err := config.Set(s(a, "key"), fmt.Sprint(a["value"])); err != nil {
			return "", err
		}
		return "set " + s(a, "key"), nil
	case "unset":
		return "unset " + s(a, "key"), config.Unset(s(a, "key"))
	}
	c, err := config.Load()
	return c.Show(), err
}

// agentKey: settings an agent may change when guard.agents_can_configure is on. Never guard.*
// (the human's protections), harvester.* (shell commands the daemon runs), budget.*, reserve.* or
// preset (a preset sets reserves).
func agentKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	return strings.HasPrefix(k, "order.") || strings.HasPrefix(k, "pricing.") || strings.HasPrefix(k, "gpu.")
}

func jobID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// callerProject is the project of the directory the agent runs this MCP server in.
func callerProject() string {
	wd, _ := os.Getwd()
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return filepath.Base(d)
		}
		if filepath.Dir(d) == d {
			return filepath.Base(wd)
		}
	}
}

// readLine reads one newline-terminated message, refusing ones over max bytes (they're skipped).
func readLine(in *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := in.ReadSlice('\n')
		if len(buf)+len(chunk) <= max {
			buf = append(buf, chunk...)
		} else {
			buf = buf[:0] // too big: drop it, keep reading to its end
			max = -1
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if max < 0 {
			return []byte{}, err
		}
		return buf, err
	}
}
