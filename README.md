<p align="center"><img src="assets/logo.svg" alt="firefighter: GPU, CPU and RAM scheduler for AI coding agents" width="640"></p>

<p align="center">
  <a href="https://github.com/JoshWright22/firefighter/actions/workflows/ci.yml"><img src="https://github.com/JoshWright22/firefighter/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/JoshWright22/firefighter/releases"><img src="https://img.shields.io/github/v/release/JoshWright22/firefighter?color=ff2d95&label=release" alt="release"></a>
  <img src="https://img.shields.io/badge/Windows%20%C2%B7%20macOS%20%C2%B7%20Linux-ff6a00" alt="Windows, macOS, Linux">
  <img src="https://img.shields.io/badge/MCP-server-9d4eff" alt="MCP server">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-ffc94a" alt="MIT"></a>
</p>

<h3 align="center">Your AI agents share the GPU. You stay the headliner.</h3>

<p align="center"><b>firefighter</b> is a local GPU, CPU and RAM scheduler, job queue and market maker for AI coding agents
(<b>Claude Code, Codex, Gemini CLI, Cursor, T3 Code</b>) that share one computer.<br>
One command to install, on <b>Windows, macOS and Linux</b>. After that, every agent session queues its heavy work, sees
what the others are doing, and gets out of the way when you need the machine.</p>

<p align="center"><img src="assets/demo.svg" alt="Animated demo: agents queue for the GPU, the human starts a game, training pauses, everyone finishes" width="860"></p>

---

## The problem

Run three or four agents at once and each one decides on its own to train a model, run an eval
sweep, build a Docker image or render something. They all grab the same GPU. VRAM runs out (CUDA
out-of-memory, or silent spill into system RAM on Windows), two trainings slow each other to a
crawl, your game stutters, audio crackles, and nobody knows who's using what or for how long.

## What firefighter does

- 🎤 **You always come first.** It watches your keyboard and mouse, fullscreen apps, games (3D-engine
  use), and every process it didn't start: Docker containers, Kubernetes pods, local model servers,
  your own apps. It keeps a **dynamic reserve** of CPUs, RAM and VRAM for you that grows within half a
  second when you sit down or start a game and gives capacity back slowly when you leave.
- 🎟️ **Agents take turns instead of fighting.** Heavy work waits in a queue. A GPU job starts only
  when its VRAM need fits, and that need is learned from the command's past runs. Projects that have
  used the least go first (least attained service), with a 45-minute cap on any wait.
- 💹 **A market maker, not a bidding war.** Every answer firefighter gives an agent includes a quote:
  the posted congestion price, what each bid level buys (start time and cost), the odds the GPU is
  busy over the next 2 hours, the cheapest upcoming window, and the best-value choice. Agents see
  the whole market in one line, don't have to look anything up, and gain nothing by gaming it.
- 🛡️ **Fallbacks for careless or rogue agents.** Heavy processes an agent starts outside firefighter
  are found and adopted anyway: they get idle priority, are charged to their project, and are
  preempted first. Other guards cover queue flooding, chronic under-estimates, VRAM hoarding, and
  jobs that run past their lease.
- ⏸️ **Polite preemption.** The ladder goes: shrink the job's CPU set → cooperative pause at a step
  boundary → bounded suspend → checkpoint-and-exit with automatic requeue → kill, but only when RAM
  is critical.
- 🔌 **Set and forget.** `setup` starts the guard at logon. It also registers the MCP server with
  Claude Code, Codex, Gemini CLI and Cursor and adds a short rule block to their global instructions,
  so every new session (including T3 Code threads) already knows the protocol.

## Install (one command)

**Windows** (PowerShell):

```powershell
irm https://raw.githubusercontent.com/JoshWright22/firefighter/main/scripts/install.ps1 | iex
```

**macOS / Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/JoshWright22/firefighter/main/scripts/install.sh | sh
```

The installer checks the release checksum, puts one ~8 MB binary on your PATH, and runs `firefighter setup`.
Autostart uses Task Scheduler on Windows, launchd on macOS and a systemd user unit on Linux. Undo
everything with `firefighter uninstall`. With Go installed:
`go install github.com/JoshWright22/firefighter/cmd/firefighter@latest && firefighter setup`.

## Use

```sh
firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
firefighter run --minutes 5 -- npm run build                          # long CPU work
firefighter run --gpu --minutes 20 -- docker run --gpus all my/eval    # containers are governed too
firefighter run --detach --gpu --minutes 90 -- python sweep.py         # returns an id; then: firefighter wait <id>
firefighter quote --gpu --minutes 30 --vram 8                          # the market, no side effects
firefighter bid <id> 7                                                 # re-bid a waiting job
firefighter status                                                     # who's on stage until when, the queue, budgets
firefighter plan "eval sweep" --when tonight --minutes 120 --gpu 90 --vram 12
firefighter extend <id> 15                                             # running long? say so
```

`run` waits for a turn, then runs your command **in your own environment** (venv, conda, uv, nvm and
docker all just work), passes its output and exit code through, and ends with a receipt:

```
🔥 nanoGPT train [main set] waiting in the wings #2 in line, on ~14:05: GPU turn queue
   market gpu: price x1.40, busy 15m 80% 60m 35% 120m 12% | bid 1: ~31m, cost 8.4 | bid 5: ~22m, cost 42.0 | bid 10: ~6m, cost 84.0 | best value: bid 5 (~22m)
🔥 nanoGPT train: spotlight's yours (lease 40 min, budget left 212.0)
🔥 nanoGPT train: curtain call, ok (exit 0) in 37.2 min (declared 40), waited 6.1, charged 31.4 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
```

**MCP tools:** `firefighter_status`, `firefighter_quote`, `firefighter_submit`, `firefighter_wait`,
`firefighter_bid`, `firefighter_extend`, `firefighter_cancel`, `firefighter_plan`, `firefighter_config`.

## How it decides (and how to change it)

Zero config gives you the researched defaults below. You or your agents can swap each part by name,
live, with `firefighter config set <key> <value>` or the `firefighter_config` MCP tool.

| Stage | Default | What it does | Swap with |
|---|---|---|---|
| **You** | dynamic reserve | Four states: away, present, active and game. The reserve grows within one 0.5 s tick and gives back 1 CPU every 15 s, RAM after 60 s and VRAM after 120 s. Jobs are pinned to the CPUs outside your reserve, after PerfIso's idle-core buffer. | `reserve.<state>.cpus / ram_pct / vram_gb / pause_gpu`, `reserve.exclusive_apps` |
| **Order** | `las` | The project with the least use goes first, with a 12 h half-life (Tiresias, Slurm fair share). A 45-minute wait cap overrides it. | `order.policy = las \| stride \| lottery \| drf \| fifo \| srpt` |
| **Market** | `priced` | Posted congestion price (×0.5 idle up to ×4 contended), locked when the job is granted. A bid costs `bid/5` and buys at most 30 min of lead. | `pricing.policy = priced \| free`, `pricing.max_lead_minutes` |
| **Budget** | `karma` | Steady refill of 240 machine-minutes a day, banked up to 2 days (Karma, OSDI '23). An empty budget drops the project to the spot band. | `budget.policy = karma \| daily \| none`, `budget.projects.<name>`, `budget.weights.<name>` |
| **Charge** | DRF | One machine-minute is all of the GPU, VRAM, RAM or CPUs for one minute (Dominant Resource Fairness). Idle VRAM is charged ×3 (ESX idle tax), overruns ×2, and chronic under-estimates ×1.25. | built in |
| **Admission** | learned peaks | A job's need is the larger of its declared size and 1.2 × its learned peak, checked against what's left after your reserve. Short jobs may backfill behind a head that's waiting on VRAM. | `gpu.max_jobs`, `gpu.pack_below_util`, `gpu.vram_margin` |
| **Preemption** | ladder | CPU set → pause file → 60 s suspend → checkpoint file with 60 s grace, then requeue → kill when RAM is critical. Spot jobs and jobs past their lease go first. | `ladder.*` |
| **Guard** | on | Adopts heavy processes started by agent CLIs outside firefighter, and caps each project at 20 open jobs. | `guard.*` |

**Presets:** `balanced` (default), `gamer`, `throughput`, `fair` → `firefighter config set preset gamer`.

| Band | Who | Preempted by |
|---|---|---|
| 0 · headliner | you, plus everything not run by firefighter (apps, games, Docker, k8s) | nothing |
| 1 · opener | short CPU jobs (≤ 3 min) | you |
| 2 · main set | in-budget GPU and long jobs (the market) | you |
| 3 · afterparty | over budget, `--spot`, past the lease, or adopted | anything |
| 4 · harvester | optional idle-GPU program (Salad, BOINC, Folding@home) | anything |

### Long GPU jobs: pause politely

Windows has no way to checkpoint a CUDA process from outside the job, so the job itself has to
cooperate. `firefighter protocol` prints a 15-line helper: call it once per step, and the job
pauses while you game and checkpoints and exits (code 75) when you need the memory. firefighter then
puts it back in line.

## Per-OS support

| | Windows | macOS | Linux |
|---|---|---|---|
| Your input | GetLastInputInfo | IOHIDSystem idle time | GNOME idle monitor, xprintidle |
| Games and fullscreen | fullscreen foreground window, 3D-engine use per process, exclusive apps | exclusive apps | exclusive apps |
| GPU / VRAM | nvidia-smi + WDDM per-process GPU counters | unified memory | nvidia-smi, rocm-smi |
| Job priority | idle CPU, idle GPU scheduling, very low memory and I/O priority | nice 19 + background QoS | nice 19, SCHED_IDLE, idle I/O |
| CPU reserve | affinity | QoS | affinity |
| No orphans | Job Object, kill on close | process group | process group |

## How it compares

| | firefighter | AgentLoom / Anvil / gpuq | pueue / task-spooler | KAI-Scheduler / Kueue / Slurm |
|---|---|---|---|---|
| Runs on Windows, macOS and Linux | ✅ | ❌ Linux/macOS only | partial | Linux clusters |
| Protects the human at the keyboard | ✅ dynamic reserve, game detection | ❌ | ❌ | ❌ |
| GPU, CPU and RAM together | ✅ DRF | GPU only | ❌ | ✅ |
| Agent protocol (MCP + rules in agent instructions) | ✅ automatic | partial | ❌ | ❌ |
| Market information for every agent | ✅ quotes, busy odds | ❌ | ❌ | ❌ |
| Catches agents that skip the queue | ✅ adoption | ❌ | ❌ | n/a |
| One-command install, no cluster | ✅ | ✅ | ✅ | ❌ |

## FAQ

**How do I stop several Claude Code or Codex agents from fighting over my GPU?** Install
firefighter. New sessions read the rule block and register the MCP server, so they queue GPU work
through `firefighter run` or `firefighter_submit` and wait their turn.

**Will it slow down my games?** It's designed to do the opposite. Fullscreen apps and 3D-engine use
switch you to the "game" state. Running GPU jobs get a cooperative pause, and if they ignore it they
are suspended and then checkpointed. No new GPU jobs start until you're done.

**Does it work with Docker and Kubernetes?** Containers you run yourself count as your load and are
protected by the reserve. `firefighter run -- docker run ...` labels the container so firefighter can
pin, pause and stop it like any other job.

**What if an agent ignores the rules?** If an agent CLI starts something that stays heavy for 60
seconds outside firefighter, it gets adopted. It's put on idle priority, charged to its project, and
evicted first under pressure.

**Can agents cheat the market?** Prices are posted, not auctioned, so there's no bidding game. A bid
of 10 costs double and buys at most 30 minutes of lead. Budgets belong to projects, so running more
jobs doesn't buy more turns. Runtimes are checked against each command's history.

**Does it need an NVIDIA GPU?** No. It reads NVIDIA (nvidia-smi plus Windows GPU counters) and AMD
(rocm-smi). On Apple silicon it schedules unified memory as RAM. Without a GPU it still schedules CPU
and RAM.

## Research

The policy comes from a survey of about 40 repos and 30 papers. Among them: DRF (NSDI '11), Tiresias
(NSDI '19), Themis (NSDI '20), Gavel (OSDI '20), Karma (OSDI '23), PerfIso (ATC '18), PilotFish
(ATC '22), lottery and stride scheduling (OSDI '94), BOINC, KAI-Scheduler, AgentCgroup and AIOS. The
full report with sources is in [docs/research.md](docs/research.md).

## Testing

`go test ./...` runs the unit tests and an end-to-end suite. The suite builds the binary, starts a
real daemon, and scripts the human, memory pressure and a GPU through `FIREFIGHTER_FAKE_FILE`. It
covers GPU turns that never overlap, VRAM admission, game pause and resume, RAM-pressure checkpoint
and requeue, cancel, quotes and re-bids, adoption of rogue processes, daemon restart, the MCP server,
and setup/uninstall. CI runs it on Windows (x64 and ARM), macOS and Linux (x64 and ARM) VMs, then
tests both one-command installers.

<p align="center"><img src="assets/icon.svg" width="72" alt=""><br><sub>MIT · built for people who run too many agents at once</sub></p>
