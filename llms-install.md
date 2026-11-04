# Installing firefighter (for AI agents)

firefighter is one binary: a local daemon plus an MCP server (`firefighter mcp`, stdio). No API keys,
no admin rights, no network service. Windows 10/11, macOS 12+ or Linux, x64 or arm64.

## 1. Install the binary

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

A new terminal may be needed before `firefighter` is on the PATH; until then use the full binary path above.

## 3. Add the MCP server

`firefighter setup` registers itself with the agent CLIs it finds (Cline CLI included). For the Cline
VS Code extension, add this to `cline_mcp_settings.json` (MCP Servers > Configure), using the full
binary path from step 1:

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

Tools: `firefighter_submit`, `firefighter_wait`, `firefighter_status`, `firefighter_job`,
`firefighter_plan`. To check it, call `firefighter_status`. Every reply ends with `do: ...`; follow it.

Remove everything with `firefighter uninstall`.
