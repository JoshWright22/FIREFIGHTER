# Installing firefighter (for AI agents)

firefighter is one binary: a local daemon plus an MCP server (`firefighter mcp`, stdio). No API keys,
no admin rights, no network service. Windows 10/11, macOS 12+ or Linux, x64 or arm64.

## 1. Install the binary

If `firefighter` is already installed (`firefighter version` prints a version), skip to step 2.

Run the one-liner for the user's OS. It checks the release checksum, puts the binary on the PATH and
runs `firefighter setup` (start at logon, rules and MCP entries for the agent CLIs it finds).

- Windows (PowerShell):
  `irm https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.ps1 | iex`
  Binary: `%LOCALAPPDATA%\Programs\firefighter\firefighter.exe`
- macOS / Linux:
  `curl -fsSL https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.sh | sh`
  Binary: `~/.local/bin/firefighter`
- With Go 1.23+ instead:
  `go install github.com/JoshWright22/firefighter/cmd/firefighter@latest`
  Binary: `$(go env GOPATH)/bin/firefighter`

## 2. Set up and check

```sh
firefighter setup    # already done by the one-liners; needed after go install. Safe to re-run.
firefighter status   # should print the machine, the queue and a "do:" line
```

A new terminal may be needed before `firefighter` is on the PATH; until then use the full path:
Windows `& "$env:LOCALAPPDATA\Programsirefighterirefighter.exe" setup`, macOS/Linux `~/.local/bin/firefighter setup`.

## 3. Add the MCP server

`firefighter setup` registers itself with the agent CLIs it finds. For the Cline CLI it writes
`~/.cline/data/settings/cline_mcp_settings.json` (Windows: `%USERPROFILE%\.cline\data\settings\`);
`cline config mcp` should list `firefighter [stdio]`. For the Cline VS Code extension, add this to its
`cline_mcp_settings.json` (MCP Servers > Configure), using the full binary path from step 1:

```json
{
  "mcpServers": {
    "firefighter": {
      "command": "<path to firefighter>",
      "args": ["mcp"]
    }
  }
}
```

On Windows, write the path with escaped backslashes, e.g.
`"C:\\Users\\<you>\\AppData\\Local\\Programs\\firefighter\\firefighter.exe"`.

MCP servers load when a Cline task starts: after setup, start a new task (or reload the VS Code
window) before calling the tools. With Cline's Claude Code provider, tools come from Claude Code's MCP
config, which `firefighter setup` fills too.

Tools: `firefighter_submit`, `firefighter_wait`, `firefighter_status`, `firefighter_job`,
`firefighter_plan`. To check it, call `firefighter_status`. Every reply ends with `do: ...`; follow it.

Remove everything with `firefighter uninstall`.
