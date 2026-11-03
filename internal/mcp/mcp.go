// Package mcp serves firefighter to agents over the Model Context Protocol (stdio, JSON-RPC 2.0).
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
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

var tools = []tool{
	{"firefighter_status", "What's using this shared machine right now: the human's state and reserve, running jobs and when each should end, the queue with start estimates, budgets, planned work. Check before heavy work.", obj(map[string]any{})},
	{"firefighter_submit", "Queue a heavy command (GPU work, or CPU work over about a minute) for a turn on this shared machine. Returns a job id at once; the command runs when its turn comes. Then call firefighter_wait.", obj(map[string]any{
		"command": str("Shell command to run"),
		"cwd":     str("Working directory (absolute)"),
		"minutes": num("Honest runtime estimate in minutes (a lease: past 1.25x it is charged double unless extended)"),
		"gpu":     boo("Needs the GPU"),
		"vram_gb": num("Peak VRAM in GB if known"),
		"ram_gb":  num("Peak RAM in GB if known"),
		"bid":     num("Urgency 1-10, default 5. Higher costs more budget: bid high only when the human waits on it"),
		"name":    str("Short name: '<project> <what>'"),
		"evict":   str("graceful (default: honors the checkpoint file) | force (may be killed at once)"),
		"spot":    boo("Run only on leftovers; cheapest, preempted first"),
	}, "command", "cwd", "minutes")},
	{"firefighter_wait", "Wait for a submitted job: returns its state, queue position and estimated start, or its receipt and log tail when done. Waits up to timeout_s seconds (default 50); call again if still running.", obj(map[string]any{
		"id": str("Job id"), "timeout_s": num("Seconds to wait (max 55)"),
	}, "id")},
	{"firefighter_quote", "The market for this machine, no side effects: posted price now, what each bid level (1-10) buys (start time, cost in machine-minutes), the odds the GPU or CPU is busy in 15/30/60/120 min, the cheapest upcoming window, and advice. firefighter_submit and firefighter_wait already include it.", obj(map[string]any{
		"gpu": boo("Needs the GPU"), "minutes": num("Runtime estimate"), "vram_gb": num("VRAM GB"), "ram_gb": num("RAM GB"), "project": str("Project (folder name)"),
	}, "minutes")},
	{"firefighter_bid", "Change the bid (1-10) of a waiting job after reading the market. Higher bids start sooner and cost more budget.", obj(map[string]any{
		"id": str("Job id"), "bid": num("New bid 1-10"),
	}, "id", "bid")},
	{"firefighter_extend", "Extend a running job's lease by more minutes, so it isn't moved to the spot band or charged double.", obj(map[string]any{
		"id": str("Job id"), "minutes": num("Extra minutes"),
	}, "id", "minutes")},
	{"firefighter_cancel", "Cancel a waiting or running job.", obj(map[string]any{"id": str("Job id")}, "id")},
	{"firefighter_plan", "Announce heavy work coming later so other agents can plan around it (reserves nothing). Use done=true with id or project to remove.", obj(map[string]any{
		"what": str("What and why"), "when": str("When"), "project": str("Project"), "minutes": num("Minutes"),
		"gpu_pct": num("GPU %"), "vram_gb": num("VRAM GB"), "ram_gb": num("RAM GB"), "done": boo("Remove the plan"), "id": str("Plan id to remove"),
	})},
	{"firefighter_config", "Read or change how firefighter manages agents. action: show | set | unset | presets. Keys are dotted, e.g. order.policy (las|stride|lottery|drf|fifo|srpt), pricing.policy (priced|free), budget.policy (karma|daily|none), budget.daily_minutes, budget.projects.<name>, budget.weights.<name>, gpu.max_jobs, preset (balanced|gamer|throughput|fair), reserve.active.cpus.", obj(map[string]any{
		"action": str("show | set | unset | presets"), "key": str("Dotted key"), "value": str("New value"),
	}, "action")},
}

// Serve runs the MCP server on stdin/stdout until stdin closes.
func Serve(version string) error {
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var req rpc
			if json.Unmarshal(line, &req) != nil {
				_ = out.Encode(rpc{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			} else if req.ID != nil {
				_ = out.Encode(handle(req, version))
			}
		}
		if err == io.EOF {
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
			"instructions":    "This machine is shared by AI agents and a human who always comes first. Submit heavy work (GPU, or CPU over a minute) with firefighter_submit and an honest minutes estimate, then firefighter_wait. Check firefighter_status first.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{-32602, err.Error()}
			return resp
		}
		text, err := call(p.Name, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}
		} else {
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
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

func js(v any) string { o, _ := json.MarshalIndent(v, "", " "); return string(o) }

func call(name string, a map[string]any) (string, error) {
	if name == "firefighter_config" {
		return configTool(a)
	}
	c, err := client.Connect()
	if err != nil {
		return "", err
	}
	switch name {
	case "firefighter_status":
		st, err := c.Status()
		return js(st), err
	case "firefighter_submit":
		if s(a, "command") == "" || f(a, "minutes") <= 0 {
			return "", fmt.Errorf("command and minutes are required")
		}
		if bid := f(a, "bid"); bid != 0 && (bid < 1 || bid > 10 || bid != float64(int(bid))) {
			return "", fmt.Errorf("bid must be a whole number 1-10")
		}
		if ev := s(a, "evict"); ev != "" && ev != "graceful" && ev != "force" {
			return "", fmt.Errorf("evict must be graceful or force (never is for the human's CLI only)")
		}
		if fi, err := os.Stat(s(a, "cwd")); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("cwd %q isn't a folder", s(a, "cwd"))
		}
		args := []string{"--minutes", fmt.Sprint(f(a, "minutes"))}
		if b(a, "gpu") {
			args = append(args, "--gpu")
		}
		if b(a, "spot") {
			args = append(args, "--spot")
		}
		for _, k := range []string{"vram_gb", "ram_gb", "bid"} {
			if v := f(a, k); v > 0 {
				args = append(args, "--"+strings.TrimSuffix(k, "_gb"), fmt.Sprint(v))
			}
		}
		if v := s(a, "name"); v != "" {
			args = append(args, "--name", v)
		}
		if v := s(a, "evict"); v != "" {
			args = append(args, "--evict", v)
		}
		shell := []string{"sh", "-c"}
		if runtime.GOOS == "windows" {
			shell = []string{"cmd", "/c"}
		}
		args = append(args, "--")
		args = append(args, shell...)
		args = append(args, s(a, "command"))
		id, log, err := client.Detached(args, s(a, "cwd"))
		if err != nil {
			return "", err
		}
		v, _ := c.GetSoon(id)
		return js(map[string]any{"id": id, "log": log, "state": v.State, "position": v.Position, "starts": v.ETA,
			"market": v.Market, "next": "call firefighter_wait with this id; re-bid with firefighter_bid if the market says so"}), nil
	case "firefighter_wait":
		t := f(a, "timeout_s")
		if t <= 0 || t > 50 {
			t = 45 // stays under common 60 s tool timeouts
		}
		deadline := time.Now().Add(time.Duration(t * float64(time.Second)))
		id := s(a, "id")
		v, err := c.GetSoon(id)
		for err == nil && (v.State != "done" || (v.Receipt != nil && v.Receipt.Requeue)) {
			left := int(time.Until(deadline).Seconds())
			if left < 1 {
				break
			}
			if v.State == "done" { // evicted and requeued: the next attempt registers shortly
				time.Sleep(500 * time.Millisecond)
				v, err = c.GetSoon(id)
				continue
			}
			v, err = c.Get(id, v.State, min(20, left))
		}
		if err != nil {
			return "", fmt.Errorf("%v\n--- log tail ---\n%s", err, client.Tail(client.LogPath(id), 1500))
		}
		out := js(v)
		if v.State == "done" {
			out += "\n--- log tail ---\n" + client.Tail(client.LogPath(id), 3000)
		}
		return out, nil
	case "firefighter_quote":
		q, err := c.Quote(b(a, "gpu"), f(a, "minutes"), f(a, "vram_gb"), f(a, "ram_gb"), s(a, "project"), "")
		return js(q), err
	case "firefighter_bid":
		v, err := c.Act(s(a, "id"), "bid", daemon.Act{Minutes: f(a, "bid")})
		return js(v), err
	case "firefighter_extend":
		v, err := c.Act(s(a, "id"), "extend", daemon.Act{Minutes: f(a, "minutes")})
		return js(v), err
	case "firefighter_cancel":
		v, err := c.Act(s(a, "id"), "cancel", daemon.Act{})
		return js(v), err
	case "firefighter_plan":
		if b(a, "done") {
			key := s(a, "id")
			if key == "" {
				key = s(a, "project")
			}
			return "removed", c.Unplan(key)
		}
		p, err := c.Plan(daemon.Plan{Project: s(a, "project"), What: s(a, "what"), When: s(a, "when"), Minutes: f(a, "minutes"),
			GPUPct: f(a, "gpu_pct"), VRAMGB: f(a, "vram_gb"), RAMGB: f(a, "ram_gb")})
		return js(p), err
	}
	return "", fmt.Errorf("unknown tool %s", name)
}

func configTool(a map[string]any) (string, error) {
	act := s(a, "action")
	if act == "set" || act == "unset" {
		c, _ := config.Load()
		if !c.Guard.AgentsCanConfigure {
			return "", fmt.Errorf("agents can read the policy but not change it; ask the human to run `firefighter config set %s ...` (or allow agents: guard.agents_can_configure)", s(a, "key"))
		}
		if strings.HasPrefix(s(a, "key"), "guard.") {
			return "", fmt.Errorf("guard.* settings are the human's only")
		}
	}
	switch act {
	case "set":
		if err := config.Set(s(a, "key"), fmt.Sprint(a["value"])); err != nil {
			return "", err
		}
		return "set " + s(a, "key") + "; the guard picks it up within a second", nil
	case "unset":
		return "unset " + s(a, "key"), config.Unset(s(a, "key"))
	case "presets":
		return "balanced (default), gamer, throughput, fair. Set with key=preset.", nil
	}
	c, err := config.Load()
	return c.Show(), err
}
