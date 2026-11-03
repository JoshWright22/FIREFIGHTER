# AGENTS.md

## Using firefighter (any project, any agent)

If `firefighter` is installed, the machine is shared and the human comes first. Run anything heavy
(GPU work, or CPU work over about a minute) through `firefighter run --minutes N [--gpu] -- <cmd>`.
Read the market line it prints, take the best-value bid unless the human is waiting, and tell the
human your place and start time if you have to wait. The full rules are the block `firefighter setup`
writes to your global instructions (see `internal/setup/setup.go`, `Instructions`).

## Working on this repo

- Go 1.23+, no cgo. Build: `go build ./cmd/firefighter`. Test: `go test ./...` (the end-to-end
  suite in `cmd/firefighter` starts real daemons in temp homes; ~3 minutes).
- Layout: `internal/policy` is pure scheduling math (ordering, prices, budgets, reserve, market,
  no I/O); `internal/daemon` is the control loop, admission, preemption ladder and HTTP API;
  `internal/sysinfo` holds per-OS probes and controls (`_windows.go`, `_linux.go`, `_darwin.go`,
  `_unix.go`); `internal/setup` handles autostart, MCP registration and agent instruction blocks;
  `internal/mcp` is the stdio MCP server; `cmd/firefighter` is the CLI.
- Keep `go vet` clean for `GOOS=windows`, `linux` and `darwin`.
- New policy knobs go in `internal/config` with a default, a validation rule and a README row.
- Tests that need a human, a GPU or memory pressure script them with `FIREFIGHTER_FAKE_FILE`.
