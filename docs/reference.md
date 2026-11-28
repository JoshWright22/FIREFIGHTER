# FIREFIGHTER reference

Install details, every command, the files it keeps, per-OS support, editor integrations, how it
compares with other tools, the FAQ and how it's tested. For how it decides, see
[how-it-works.md](how-it-works.md); for the trust model, see [security.md](security.md).

## Install details

The one-command installers are in the [README](../README.md#install-one-command).

**Requirements:** Releases target Windows, macOS and Linux on x64 and arm64. No admin rights needed.
GPU reporting depends on the platform; see [Per-OS support](#per-os-support). On Linux, idle detection
uses GNOME, a desktop with the freedesktop ScreenSaver interface, or `xprintidle` on X11. With no idle
reading, the guard counts you as "present".

**What install changes** (setup makes a one-time `*.firefighter.bak` for existing agent files it rewrites directly):

| What | Where |
|---|---|
| Binary | `%LOCALAPPDATA%\Programs\firefighter` (user PATH) or `~/.local/bin` (the Unix installer adds it to existing `.bashrc`, `.zshrc` or `.profile` files when needed) |
| Autostart | Windows installer's `firefighter.lnk` in the current user's Startup folder (`shell:startup`) · `~/Library/LaunchAgents/dev.firefighter.plist` · `~/.config/systemd/user/firefighter.service` on Linux when user systemd is available, otherwise `~/.config/autostart/firefighter.desktop` (needs a desktop that honors XDG autostart) |
| Agent rules | a marked block in supported instruction files, such as `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` or `~/.gemini/GEMINI.md` |
| MCP server | `claude mcp add` (or a direct config edit), `~/.codex/config.toml`, `~/.gemini/settings.json`, `~/.cursor/mcp.json` and other supported agent configs |
| State | `%LOCALAPPDATA%\firefighter`, `~/Library/Application Support/firefighter`, `~/.local/state/firefighter` (or `FIREFIGHTER_HOME`) |

**Verify:** open a new shell on macOS or Linux to pick up any PATH change, then run `firefighter status`.
`go install` puts the binary in Go's bin directory and reports version `dev`, which does not self-update.
On Windows, the PowerShell installer creates the shortcut and removes legacy logon entries.
To migrate an older Windows install, rerun the PowerShell installer from a release that includes
the Startup shortcut.
The CLI can start the guard on demand with or without the shortcut.

**Remove:** `firefighter uninstall` removes autostart, the rule blocks and the MCP entries, and stops
the guard. It leaves the binary, PATH and state folder alone. Delete the installed binary. If you no
longer need its bin directory on PATH, remove the installer-added entry from your Windows user PATH
or shell startup files. Don't remove a shared entry you still use. Delete the state folder to remove
settings, budgets and history.

## Using it

```sh
firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
firefighter run --gpu --urgent --minutes 10 -- python eval.py          # you're waiting on it: 2x by default
firefighter run --gpu --spot --minutes 120 -- python sweep.py          # can wait: 1/3 price by default
firefighter run --minutes 5 -- npm run build                          # long CPU work
firefighter run --gpu --minutes 20 -- docker run --gpus all my/eval    # containers are governed too
id=$(firefighter run --detach --gpu --minutes 90 -- python sweep.py) && firefighter wait "$id"
firefighter quote --gpu --minutes 30 --vram 8                          # prices only, queues nothing
firefighter bid <id> 10                                                # make a waiting job urgent (5 = normal)
firefighter status                                                     # who's on stage until when, the queue, budgets
firefighter plan "eval sweep" --when 22:00 --minutes 120 --gpu 90 --vram 12
firefighter extend <id> 15                                             # running long? say so
```

In the foreground, `run` waits for a turn, runs the command in your environment and passes its
output through. It prints a receipt for each completed attempt and returns the final exit code:

```
◆ 3f2a91c0 nanoGPT train waiting #2, starts ~2:05 PM (in 22m): GPU turn queue
   gpu 40m: start ~22m, cost ~42 of 212 (urgent ~6m ~84) (spot ~22m ~14) | busy 15/60/120m 79/35/12% | quiet 02:00 (usually 8% busy) | do: queue it at normal (starts ~22m); keep working meanwhile
◆ nanoGPT train: spotlight's yours (lease 40 min, budget left 212.0)
◆ nanoGPT train: curtain call, ok (exit 0) in 37.2 min (declared 40), waited 22.0, charged 31.4 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
```

**MCP tools:** Six by default: `firefighter_submit`, `firefighter_wait`, `firefighter_status`,
`firefighter_job`, `firefighter_plan` and `firefighter_storage`. `firefighter_config` is available
only when `guard.agents_can_configure` is true.

`firefighter_job` actions are `extend`, `urgent`, `normal`, `amend` and `cancel`.
`firefighter_storage` actions are `show`, `claim`, `label`, `items`, `free` and `release`.
Pass `cwd` (your absolute working directory) for `claim`, `label`, `free` and `release`. To change
another project's space, also pass `force=true`.

## CLI reference

| Command | What it does |
|---|---|
| `run [flags] -- <cmd...>` | Wait for a turn, run a command and print a receipt. See flags below. |
| `wait <id> [--timeout s]` | Follow a job through requeues. Prints its JSON receipt and a log tail if available. Returns the job's exit code, or 124 on timeout. |
| `quote [--gpu] [--minutes N] [--vram GB] [--ram GB] [--urgent] [--json] [-- <cmd...>]` | Prices a job (30 minutes by default). An optional command lets it use matching history. May start the guard, but queues nothing. |
| `bid <id> <1-10> [--force]` | Change a waiting job's bid. 5 is normal, 10 is urgent. |
| `amend <id> [--ram GB] [--vram GB] [--minutes N] [--urgent] [--force]` | Change a waiting job's estimates or mark it urgent without resetting its wait. |
| `extend <id> <minutes> [--force]` · `cancel <id> [--force]` | Lengthen a running job's lease or cancel a waiting or running job. |
| `status [--json]` | The human's state and reserve, the market, running and waiting jobs, budgets, plans |
| `update [--check]` | On a release build, install a newer release and restart the guard. `--check` reports without installing. Release builds also check and update automatically unless `update.auto` is `false`. |
| `forecast [--json]` | What has been learned: the human's use and reserve per state, and what each known command is expected to need on its next run (RAM, VRAM, CPUs, GPU load, disk, network, minutes) |
| `adopt <pid> [--gpu] [--name NAME] [--minutes N] [--force]` | Govern a process that's already running. `--force` allows adoption outside the caller's agent session. |
| `plan "<what>" --minutes N [--when now\|HH:MM] [--gpu PCT] [--vram GB] [--ram GB] [--project P]` | Announce work without reserving resources. The forecast reads `now` (the default), `HH:MM` and 12-hour times like `9pm`; other words are shown but not forecast. |
| `plan [<id>] --done [--project P]` | Remove one plan by ID or all plans for the project. |
| `harvest [earned <usd> [--through <date-or-time>] [--note TEXT] \| reset]` | Show power costs and earnings (`show` and `status` are aliases). `earned` adds income since the last report; `--through` accepts `YYYY-MM-DD` or `"YYYY-MM-DD HH:MM"` and defaults to now. `reset` clears the profit-guard trip. |
| `storage [show\|status\|ls] [--json]` | Show the allotment and all project spaces. |
| `storage claim --gb N [--why TEXT] [--project P] [--force]` · `storage path [--project P]` | Set a project's claim and print its folder, or look up the existing folder. |
| `storage label <path> --rank keep\|useful\|scratch\|none [--for TEXT] [--project P] [--force]` | Rank a file or folder and say what needs it. `--for` is required unless removing a label with `none`. |
| `storage items [--json] [--project P]` | List a project's contents, least important first. |
| `storage free --gb N [--upto scratch\|unranked\|useful] [--dry-run] [--project P] [--force]` | Free the least important items first, never `keep`. Defaults to `scratch`; `--dry-run` previews the selection. |
| `storage release [--delete] [--project P] [--force]` | Give back a claim. `--delete` also removes its files. |
| `doctor` | Check the install, PATH, the guard (running, same version), settings, each agent's rule block, GPUs, disks, Docker and agent storage. Prints the fix for each problem; exits 1 if there is any. |
| `config [show \| set <key> <value> \| unset <key> \| presets \| path]` | Every key is in `config show` |
| `protocol` | The pause/checkpoint helper |
| `statusline [--json]` · `events` | One line for a status bar, or an NDJSON stream of status changes. Neither starts the guard. |
| `setup` (`install`) · `uninstall` | Add or remove integrations and start or stop the guard. |
| `daemon [--stop]` · `mcp` | Run or stop the guard, or run the MCP server on stdio. |
| `version` (`--version`, `-v`) · `help` (`--help`, `-h`) | Show the version or CLI help. |

**`run` flags:**

| Flag | What it does |
|---|---|
| `--minutes N` | Estimate runtime and set the lease. Defaults to 30 minutes, or 2 with `--quick`. |
| `--ram GB` · `--vram GB` | Declare peak RAM and VRAM. The needs used for admission can be clamped. |
| `--gpu` · `--gpu-vendor nvidia\|amd\|intel\|any` | Request GPU scheduling and optionally restrict which cards may run the job. Defaults to NVIDIA if present. |
| `--net` | Mark the job as network-heavy. Known downloads and installs can also be detected automatically. |
| `--quick` | Use a lease of at most 3 minutes for a short task. Normal resource and storage checks still apply. |
| `--spot` · `--urgent` · `--bid 1-10` | Use leftover capacity, mark work the human is waiting on, or set the bid directly (5 normal, 10 urgent). By default, spot pays one third and urgent pays twice the normal rate. `pricing.policy=free` removes the bid multiplier; the spot discount is separate. |
| `--evict never\|graceful\|force` | Choose how readily a job can be stopped. `graceful` is the default; `force` skips checkpoint grace. `never` is not an exemption from the human's needs: critical RAM pressure, VRAM below the reserve for three pressure periods, and a game holding past the suspend limit still stop it. |
| `--detach` · `--quiet` | Return the job ID without waiting, or suppress routine status output. |
| `--request-id KEY` | Idempotent submit: the same key in the same project never queues a second copy; a retry gets the first job (waits on it, or prints its id with `--detach`). MCP: `request_id` on `firefighter_submit`. |
| `--name NAME` · `--project P` | Set the job and project names. The project defaults to the Git repo or folder. |

For job controls, `--force` allows changing a job outside your project or session. For `storage`,
pass `--force` when using `claim`, `label`, `free` or `release` on another project's space.

**Files** in the state folder: `firefighter.toml` (your settings), `state.json` (budgets, learned
history, receipts, the hour-of-week occupancy), `STATUS.md` (a status snapshot rewritten every 15 s
for agents that read files), `daemon.log`, `logs/<id>.log` (detached jobs, kept 7 days),
`signals/` (pause and checkpoint files), `daemon.json` and `daemon.lock`; with agent storage,
`storage.json` (claims and labels) and `storage.lock`; with a harvester, `harvest-ledger.csv`,
`harvest-earnings.csv` and `harvest-trip.txt`.

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

- **Agents `setup` finds** (see the [README](../README.md#agents-it-sets-up), including supported T3 Code
  profiles): setup adds an MCP entry, an instruction block, or both, depending on the agent.
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
| GPU, CPU and RAM together | ✅ LAS by default, DRF available | ✅ | ❌ slots | ❌ GPU only | ✅ |
| Per-card GPU scheduling | ✅ NVIDIA/AMD/Intel where detected | ❌ | ❌ | NVIDIA | ✅ |
| Agent protocol (MCP + rules in agent instructions) | ✅ automatic, ~20 agents | partial (Claude only) | load-lock ✅ ~10 CLIs | partial | ❌ |
| Market information for every agent | ✅ quotes, busy odds | partial: ETAs | ❌ | ❌ | ❌ |
| Catches agents that skip the queue | ✅ adoption | partial: tags it | ❌ | gpulease: adopts, no limits | n/a |
| Preemption ladder (pause → suspend → checkpoint → kill) | ✅ | partial: requeue | ❌ | partial | ✅ |
| One-command install, no cluster | ✅ | partial | ✅ | ✅ | ❌ |

Prior art was surveyed in Nov 2026 (about 65 projects read in full); nothing found does more than
about half of this, and none on all three OSes. Details: [research.md](research.md).

## FAQ

**How do I stop several Claude Code or Codex agents from fighting over my GPU?** Install
FIREFIGHTER. Setup registers the MCP server and adds the shared-machine rules for these agents.
New sessions can queue GPU work through `firefighter run` or `firefighter_submit`.

**Why do I get CUDA out of memory when two AI agents train at once?** Each agent sees the whole
card free and starts its own job, so together they can ask for more VRAM than the card has.
FIREFIGHTER checks the job's estimated need against what's free on one card after the reserve and
existing jobs. That estimate comes from declarations and past runs, and can be clamped to what the
card can offer. It isn't a guarantee against running out of VRAM.

**Can several AI agents share one GPU?** Yes. They take turns, or run side by side when the card
has room (`gpu.max_jobs` per card). With more than one GPU, the guard prefers an open, less loaded
card that fits and matches the job's GPU vendor choice.

**Is there an MCP server for queuing GPU jobs?** Yes. `firefighter mcp` runs it on stdio, and setup
registers it for supported agents. Use `firefighter_submit` to queue work and `firefighter_wait` for
the receipt. The full tool list is in [Using it](#using-it).

**What happens when I start a game?** Windows detects fullscreen and 3D-engine use. On macOS and
Linux, list your games in `reserve.exclusive_apps`. By default, the game state holds new GPU jobs.
Running GPU jobs get a chance to pause. The guard can suspend them, ask them to checkpoint, or stop
them if it needs their memory back. Jobs that checkpoint and exit with code 75 requeue.

**Does it work with Docker?** Docker containers you run yourself count as outside load.
`firefighter run -- docker run ...` labels the container so the guard can track, pause and stop it.
Keep `docker run` in the foreground. Use `firefighter run --detach` if you don't want to wait.

**What if an agent ignores the rules?** With auto-adopt on, the guard looks for unmanaged processes
descended from known agent CLIs (`guard.agent_parents`) that stay heavy for `guard.heavy_seconds`
(60 seconds by default). Adopted work runs at idle priority and goes first under pressure.

**Can agents cheat the market?** Prices are posted, not auctioned. By default, a bid of 10 pays twice
the normal rate and subtracts up to 30 machine-minutes from the ordering key. It doesn't guarantee
a 30-minute earlier start. Jobs in one project share a budget. The guard checks runtime estimates
against measured history when available. Agents can change the policy only if you set
`guard.agents_can_configure`, and then only `order.*` (not `order.max_wait_minutes`), `pricing.*` and
`gpu.*`, through MCP or the CLI alike: `firefighter config set` run from an agent's session refuses
the rest. An agent running as your user can still edit the files directly.
FIREFIGHTER is a referee for cooperative agents, not a security boundary.

**Does it need an NVIDIA GPU?** No. Windows also reports AMD and Intel GPUs through DXGI and GPU
counters. Linux can read AMD GPUs through `rocm-smi` or amdgpu sysfs. On Apple silicon, unified memory
counts as RAM. Without a GPU it still schedules CPU and RAM.

## Testing

`go test ./...` runs the unit tests and an end-to-end suite. The suite builds the binary, starts a
real daemon, and scripts the human, memory pressure and a GPU through `FIREFIGHTER_FAKE_FILE`. It
covers GPU turns that never overlap, VRAM admission, game pause and resume, RAM-pressure checkpoint
and requeue, cancel, quotes and re-bids, adoption of rogue processes, daemon restart, the MCP server,
and setup/uninstall. CI runs it on Windows (x64 and ARM), macOS and Linux (x64 and ARM) VMs, then
tests both one-command installers.
