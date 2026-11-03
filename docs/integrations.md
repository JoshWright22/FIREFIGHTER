# Editor and tool integrations

firefighter works with any editor or agent app through four layers. Use whichever your tool supports.

## 1. Agents: MCP + rules (automatic)

`firefighter setup` registers the MCP server (`firefighter mcp`) with Claude Code, Codex, Gemini
CLI and Cursor. It also writes the shared-machine rules into the global instructions of Claude
Code, Codex and Gemini. Apps built on those agents inherit both, so there's nothing extra to set
up for them. That includes **T3 Code**, Conductor and the Claude desktop and IDE extensions.

## 2. Status line: `firefighter statusline`

`firefighter statusline` prints one line, or a JSON object with `--json`, and never starts or
blocks on the daemon. Call it from any status bar:

```
🔥 ⌨ 1 on stage · 2 waiting · GPU x1.40 · busy 1h 35%
```

- **Claude Code** (`~/.claude/settings.json`):
  ```json
  { "statusLine": { "type": "command", "command": "firefighter statusline" } }
  ```
- **tmux**: `set -g status-right '#(firefighter statusline)'`
- **starship**: a `[custom.firefighter]` module with `command = "firefighter statusline"`

`--json` gives `{ text, user, running, waiting, gpu_price, gpu_busy_60m, next_start, tooltip }`.

## 3. Event stream: `firefighter events`

Spawn `firefighter events` once and read stdout. It writes one JSON object per line whenever the
picture changes, plus a heartbeat every 30 s:

```json
{"type":"status","time":"…","line":{…the statusline JSON…},"status":{…the full `status --json`…}}
```

It reconnects on its own if the daemon restarts. This is what the VS Code extension uses, and it's
the easiest way for any editor plugin (including a T3 Code or Zed extension) to show live state.

## 4. Editor plugins in this repo

**Claude Code plugin** ([integrations/claude-code](../integrations/claude-code)):

- a live status-line entry, refreshed every 5 s
- a `/firefighter` command that shows the full status
- a guard on the Bash tool: obviously heavy commands (training/eval scripts, `torchrun`,
  `deepspeed`, `accelerate launch`, `docker run --gpus`) that don't go through firefighter are
  sent back to the agent with the exact `firefighter run` command to use instead.
  `FIREFIGHTER_LIGHT=1` in front of a command skips the guard.

Install, at a Claude Code prompt:

```
/plugin install firefighter --marketplace JoshWright22/firefighter
```

**VS Code, Cursor, Windsurf** ([integrations/vscode](../integrations/vscode)): a status bar item
driven by `firefighter events` (turns amber while you're gaming), with commands for status, quotes
and cancelling. Each release ships `firefighter-vscode.vsix`; install it with
`code --install-extension firefighter-vscode.vsix` (`cursor` or `windsurf` work the same way).
From source: `cd integrations/vscode && npx @vscode/vsce package`.

## The HTTP API (for deeper integrations)

The daemon listens on `127.0.0.1` at the port in `<state folder>/daemon.json`. Every request
needs the header `X-Firefighter-Token: <token from daemon.json>`. The file is readable only by
you.

| Method and path | What it does |
|---|---|
| `GET /v1/ping` | `{ok, pid, version}` |
| `GET /v1/status` | Everything `status --json` shows |
| `GET /v1/quote?gpu=true&minutes=30&vram=8&ram=4&project=p` | The market for a job, with no side effects |
| `POST /v1/tickets` | Queue a job: `{project, name, gpu, minutes, vram_gb, ram_gb, bid, evict, spot, client_pid}` |
| `GET /v1/tickets/{id}?state=waiting&wait=20` | Get a job; long-polls up to `wait` seconds for its state to change |
| `POST /v1/tickets/{id}/{start\|done\|extend\|bid\|cancel\|container}` | Change a job: `{pid}`, `{exit}`, `{minutes}` (extend, bid) |
| `POST /v1/plans` · `DELETE /v1/plans/{id or project}` | Announce or remove planned work |
| `POST /v1/shutdown` | Save and exit |

The CLI (`firefighter run`) is the reference client, in `cmd/firefighter/main.go` and
`internal/client`.
