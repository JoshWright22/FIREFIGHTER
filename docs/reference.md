# FIREFIGHTER reference

Install details, every command, the files it keeps, per-OS support, editor integrations, how it
compares with other tools, the FAQ and how it's tested. For how it decides, see
[how-it-works.md](how-it-works.md); for the trust model, see [security.md](security.md).

## Install details

The one-command installers are in the [README](../README.md#install-one-command).

**Requirements:** Windows 10/11, macOS 12+ or Linux, on x64 or arm64. No admin rights needed. For
GPU numbers you need `nvidia-smi` (NVIDIA driver) or `rocm-smi` (AMD). On Linux, idle detection
uses GNOME, KDE or another freedesktop ScreenSaver desktop, or `xprintidle`; headless machines
count you as "present".

**What install changes** (each config file gets a one-time `*.firefighter.bak` backup first):

| What | Where |
|---|---|
| Binary | `%LOCALAPPDATA%\Programs\firefighter` (added to your user PATH) or `~/.local/bin` (added to your shell rc) |
| Autostart | Task Scheduler task `firefighter` (falls back to the HKCU Run key) · `~/Library/LaunchAgents/dev.firefighter.plist` · `~/.config/systemd/user/firefighter.service` (falls back to XDG autostart) |
| Agent rules | a marked block in `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, `~/.gemini/GEMINI.md` |
| MCP server | `claude mcp add` (or `~/.claude.json`), `~/.codex/config.toml`, `~/.gemini/settings.json`, `~/.cursor/mcp.json` |
| State | `%LOCALAPPDATA%\firefighter`, `~/Library/Application Support/firefighter`, `~/.local/state/firefighter` (or `FIREFIGHTER_HOME`) |

**Verify:** open a new terminal and run `firefighter status`. **Remove:** `firefighter uninstall` undoes
autostart, the rule blocks and the MCP entries. Then delete the binary and, if you want to forget
budgets and history, the state folder.

## Using it

```sh
firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
firefighter run --gpu --urgent --minutes 10 -- python eval.py          # you're waiting on it: 2x
firefighter run --gpu --spot --minutes 120 -- python sweep.py          # can wait: 1/3 price
firefighter run --minutes 5 -- npm run build                          # long CPU work
firefighter run --gpu --minutes 20 -- docker run --gpus all my/eval    # containers are governed too
id=$(firefighter run --detach --gpu --minutes 90 -- python sweep.py) && firefighter wait "$id"
firefighter quote --gpu --minutes 30 --vram 8                          # the market, no side effects
firefighter bid <id> 10                                                # make a waiting job urgent (5 = normal)
firefighter status                                                     # who's on stage until when, the queue, budgets
firefighter plan "eval sweep" --when tonight --minutes 120 --gpu 90 --vram 12
firefighter extend <id> 15                                             # running long? say so
```

`run` waits for a turn, then runs your command **in your own environment** (venv, conda, uv, nvm and
docker all just work), passes its output and exit code through, and ends with a receipt:

```
◆ 3f2a91c0 nanoGPT train waiting #2, starts ~2:05 PM (in 22m): GPU turn queue
   gpu 40m: start ~22m, cost ~42 of 212 (urgent ~6m ~84) (spot ~70m ~14) | busy 15/60/120m 92/35/12% | quiet 02:00 (usually 8% busy) | do: queue it at normal (starts ~22m); keep working meanwhile
◆ nanoGPT train: spotlight's yours (lease 40 min, budget left 212.0)
◆ nanoGPT train: curtain call, ok (exit 0) in 37.2 min (declared 40), waited 6.1, charged 31.4 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
```

**MCP tools** (one-line text replies plus `structuredContent`): `firefighter_submit`,
`firefighter_wait`, `firefighter_status` (which also gives quotes), `firefighter_job` (extend,
urgent/normal, cancel) and `firefighter_plan`. `firefighter_config` is listed only when you allow
agents to change the policy.

## CLI reference

| Command | What it does |
|---|---|
| `run [flags] -- <cmd...>` | Wait for a turn, run, receipt. Flags: `--gpu`, `--minutes N` (lease; 30 if omitted), `--vram GB`, `--ram GB`, `--urgent` (2×, the human is waiting), `--spot` (⅓, leftovers), `--name`, `--project` (default: the git repo or folder), `--evict never\|graceful\|force`, `--detach` (prints the id), `--quiet`, `--bid 1-10` (legacy; 5 normal, 10 urgent) |
| `wait <id> [--timeout s]` | Block until the job ends (following requeues); prints the receipt and log tail. Exits with the job's exit code, or 124 on timeout |
| `quote [--gpu] --minutes N [--vram GB] [--ram GB] [--urgent] [--json]` | The market for a job, no side effects |
| `bid <id> <5\|10>` · `extend <id> <minutes>` · `cancel <id>` | Change a job's tier, lease, or cancel it |
| `status [--json]` | The human's state and reserve, the market, running and waiting jobs, budgets, plans |
| `adopt <pid> [--gpu] [--name] [--minutes]` | Govern a process that's already running |
| `plan "<what>" --when ... --minutes N [--gpu %] [--vram GB] [--ram GB]` · `plan --done [--project p]` | Announce or remove planned work |
| `config [show \| set <key> <value> \| unset <key> \| presets \| path]` | Every key is in `config show` |
| `protocol` | The pause/checkpoint helper |
| `setup` · `uninstall` · `daemon [--stop]` · `mcp` · `version` | Install, remove, run or stop the guard, MCP server |

**Files** in the state folder: `firefighter.toml` (your settings), `state.json` (budgets, learned
history, receipts, the hour-of-week occupancy), `STATUS.md` (a status snapshot rewritten every 15 s
for agents that read files), `daemon.log`, `logs/<id>.log` (detached jobs, kept 7 days),
`signals/` (pause and checkpoint files), `daemon.json` and `daemon.lock`.

## Per-OS support

| | Windows | macOS | Linux |
|---|---|---|---|
| Your input | GetLastInputInfo | IOHIDSystem idle time | GNOME/Mutter, freedesktop ScreenSaver (KDE, Wayland), xprintidle |
| Games and fullscreen | fullscreen foreground window (not merely maximized), 3D-engine use per process, exclusive apps | exclusive apps only (add your games to `reserve.exclusive_apps`) | exclusive apps only (`gamescope` by default; add yours); idle time from the desktop |
| GPU / VRAM | any vendor: nvidia-smi, plus DXGI + GPU counters for AMD/Intel (no vendor tool needed) | unified memory (counted as RAM) | nvidia-smi, rocm-smi or amdgpu sysfs; outside VRAM holders count as load only while the card is busy |
| Job priority | idle CPU, idle GPU scheduling, very low memory and I/O priority | nice 19 + background QoS | nice 19, SCHED_IDLE, idle I/O class, on every thread |
| CPU reserve | affinity (CPU Sets past 64 CPUs) | QoS | affinity |
| No orphans | Job Object, kill on close | process group | process group |

## Editors and agent apps

- **Every agent `setup` finds** (see the list in the [README](../README.md#agents-it-sets-up), T3 Code's providers included): MCP and the rules
  are wired up automatically.
- **Claude Code plugin:** a live status-line entry, `/firefighter`, and a guard that sends heavy Bash
  commands back to the agent to rerun through the queue:
  `/plugin install firefighter --marketplace JoshWright22/FIREFIGHTER`
- **VS Code, Cursor, Windsurf extension:** a live status bar item (the `.vsix` is on every release).
- **Anything else:** `firefighter statusline` gives one line for any status bar (Claude Code's
  `statusLine`, tmux, starship), and `firefighter events` streams NDJSON for editor plugins.
  There's also a local HTTP API. See [integrations.md](integrations.md).

## How it compares

| | FIREFIGHTER | worker-q | load-lock / agent-task-queue | gpulease / gpu-arbiter | KAI-Scheduler / Kueue / Slurm |
|---|---|---|---|---|---|
| Runs on Windows, macOS and Linux | ✅ | partial (Windows, Linux) | ❌ macOS/Linux | ❌ Linux | Linux clusters |
| Protects the human at the keyboard | ✅ dynamic reserve, game detection | partial: static reserve, manual gaming mode | ❌ | gpu-arbiter: Steam games | ❌ |
| GPU, CPU and RAM together | ✅ DRF | ✅ | ❌ slots | ❌ GPU only | ✅ |
| Any mix of GPUs, pinned per card | ✅ NVIDIA/AMD/Intel | ❌ | ❌ | NVIDIA | ✅ |
| Agent protocol (MCP + rules in agent instructions) | ✅ automatic, ~20 agents | partial (Claude only) | load-lock ✅ ~10 CLIs | partial | ❌ |
| Market information for every agent | ✅ quotes, busy odds | partial: ETAs | ❌ | ❌ | ❌ |
| Catches agents that skip the queue | ✅ adoption | partial: tags it | ❌ | gpulease: adopts, no limits | n/a |
| Preemption ladder (pause → suspend → checkpoint → kill) | ✅ | partial: requeue | ❌ | partial | ✅ |
| One-command install, no cluster | ✅ | partial | ✅ | ✅ | ❌ |

Prior art was surveyed in Nov 2026 (about 65 projects read in full); nothing found does more than
about half of this, and none on all three OSes. Details: [research.md](research.md).

## FAQ

**How do I stop several Claude Code or Codex agents from fighting over my GPU?** Install
FIREFIGHTER. New sessions read the rule block and register the MCP server, so they queue GPU work
through `firefighter run` or `firefighter_submit` and wait their turn.

**Why do I get CUDA out of memory when two AI agents train at once?** Each agent sees the whole
card free and starts its own job, so together they ask for more VRAM than the card has. FIREFIGHTER
starts a GPU job only when its VRAM need fits on one card next to what's already running and your
reserve; the need is learned from that command's past runs.

**Can several AI agents share one GPU?** Yes. They take turns, or run side by side when the card
has room (`gpu.max_jobs` per card). With more than one GPU, each card takes jobs on its own and a job
goes to the least busy card that fits.

**Is there an MCP server for queuing GPU jobs?** `firefighter mcp`, registered for every agent setup
finds. Tools: `firefighter_submit`, `firefighter_wait`, `firefighter_status`, `firefighter_job`,
`firefighter_plan`.

**Will it slow down my games?** It's designed to do the opposite. Fullscreen apps and 3D-engine use
switch you to the "game" state (on macOS and Linux, list your games in `reserve.exclusive_apps`).
Running GPU jobs get a cooperative pause; jobs that ignore it are suspended. If your game then needs
the memory a job holds, that job checkpoints (or, if it ignored the pause, is stopped) and requeues.
No new GPU jobs start until you're done.

**Does it work with Docker and Kubernetes?** Containers you run yourself count as your load and are
protected by the reserve. `firefighter run -- docker run ...` labels the container so FIREFIGHTER can
pin, pause and stop it like any other job.

**What if an agent ignores the rules?** If an agent CLI starts something that stays heavy for 60
seconds outside FIREFIGHTER, it gets adopted. It's put on idle priority, charged to its project, and
evicted first under pressure.

**Can agents cheat the market?** Prices are posted, not auctioned, so there's no bidding game. A bid
of 10 costs double and buys at most 30 minutes of lead. Budgets belong to projects, so running more
jobs doesn't buy more turns. Runtimes are checked against each command's history. Agents can read
the policy but not change it unless you set `guard.agents_can_configure`. An agent with shell
access could still edit FIREFIGHTER's files directly. FIREFIGHTER is a referee for cooperative
agents, not a security boundary.

**Does it need an NVIDIA GPU?** No. It reads NVIDIA (nvidia-smi plus Windows GPU counters) and AMD
(rocm-smi). On Apple silicon it schedules unified memory as RAM. Without a GPU it still schedules CPU
and RAM.

## Testing

`go test ./...` runs the unit tests and an end-to-end suite. The suite builds the binary, starts a
real daemon, and scripts the human, memory pressure and a GPU through `FIREFIGHTER_FAKE_FILE`. It
covers GPU turns that never overlap, VRAM admission, game pause and resume, RAM-pressure checkpoint
and requeue, cancel, quotes and re-bids, adoption of rogue processes, daemon restart, the MCP server,
and setup/uninstall. CI runs it on Windows (x64 and ARM), macOS and Linux (x64 and ARM) VMs, then
tests both one-command installers.
