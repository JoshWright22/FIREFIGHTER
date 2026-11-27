package main

// End-to-end coverage of the MCP server.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPServer(t *testing.T) {
	e := newEnv(t, nil)
	c := e.cmd(nil, "mcp")
	in, _ := c.StdinPipe()
	outPipe, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	r := bufio.NewReader(outPipe)
	call := func(id int, method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		in.Write(append(b, '\n'))
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m
	}
	init := call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "firefighter" {
		t.Fatalf("initialize: %v", init)
	}
	in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	list := call(2, "tools/list", map[string]any{})
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 6 {
		t.Fatalf("want 6 tools (config hidden from agents by default), got %d", n)
	}
	st := call(3, "tools/call", map[string]any{"name": "firefighter_status", "arguments": map[string]any{}})
	if st["result"].(map[string]any)["isError"] == true {
		t.Fatalf("status tool: %v", st)
	}
	// ea42b3cb: the forecast made this reply 97 KB, over MCP clients' limit; it's `firefighter forecast`.
	if raw, _ := json.Marshal(st["result"]); strings.Contains(string(raw), `"forecast"`) {
		t.Fatalf("the status tool's result carries the forecast:\n%.300s", raw)
	}
	cwd, _ := os.Getwd()
	cmdline := "echo mcp-job"
	sub := call(4, "tools/call", map[string]any{"name": "firefighter_submit", "arguments": map[string]any{"command": cmdline, "cwd": cwd, "minutes": 1}})
	text := sub["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	fields := strings.Fields(text)
	if len(fields) < 2 || (fields[0] != "queued" && fields[0] != "started") || len(text) > 400 {
		t.Fatalf("submit should be one short line: %s", text)
	}
	id := fields[1]
	var waitText string
	for i := 0; i < 4 && !strings.Contains(waitText, " done "); i++ {
		w := call(5+i, "tools/call", map[string]any{"name": "firefighter_wait", "arguments": map[string]any{"id": id, "timeout_s": 20}})
		waitText = w["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	// The job's output isn't inlined (it could pass for firefighter's do: guidance); its log is named.
	if !strings.Contains(waitText, "done ok exit 0") || strings.Contains(waitText, "mcp-job") || !strings.Contains(waitText, "untrusted data") ||
		!strings.Contains(readFile(filepath.Join(e.home, "logs", id+".log")), "mcp-job") {
		t.Fatalf("wait:\n%s", waitText)
	}
	stat := call(19, "tools/call", map[string]any{"name": "firefighter_status", "arguments": map[string]any{"minutes": 20, "gpu": true}})
	stText := stat["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(stText, "human:") || !strings.Contains(stText, "quote: gpu 20m") || !strings.Contains(stText, "do: ") || len(stText) > 1500 {
		t.Fatalf("status:\n%s", stText)
	}
	cfg := call(20, "tools/call", map[string]any{"name": "firefighter_config", "arguments": map[string]any{"action": "set", "key": "budget.daily_minutes", "value": "99999"}})
	if cfg["result"].(map[string]any)["isError"] != true {
		t.Fatalf("agents must not be able to raise budgets by default: %v", cfg)
	}
	badCwd := call(22, "tools/call", map[string]any{"name": "firefighter_submit", "arguments": map[string]any{"command": cmdline, "cwd": "/no/such/dir", "minutes": 1}})
	if badCwd["result"].(map[string]any)["isError"] != true {
		t.Fatal("a missing cwd must be rejected before spawning")
	}
	bad := call(23, "nope", nil)
	if bad["error"] == nil {
		t.Fatal("unknown method must error")
	}
}
