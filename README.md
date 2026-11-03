<p align="center"><img src="assets/logo.svg" alt="firefighter" width="640"></p>

<p align="center"><b>Lets a pack of AI agents share one computer's GPU, CPU and RAM, and keeps you in front.</b><br>
Windows · macOS · Linux · one command · set and forget</p>

---

You've got Claude Code, Codex and a few T3 threads all running at once. Each one decides on its own
to train a model, run evals, build a Docker image or render something. Your GPU runs out of memory,
your game stutters, audio crackles, and two training runs slow each other to a crawl.

firefighter is a small daemon plus CLI and MCP server that sits between the agents and the machine:

- **You come first, automatically.** It watches your keyboard and mouse, fullscreen apps, games, and
  every process that isn't one of its jobs (Docker containers, Kubernetes pods, local model servers,
  your own apps). It keeps a reserve of CPUs, RAM and VRAM for you. The reserve grows the moment you
  sit down or start a game and shrinks slowly after you leave.
- **Agents take turns instead of fighting.** Heavy work waits in a queue. A GPU job starts only when
  its VRAM need (learned from its past runs) fits. The queue is ordered by fair share, so a project
  that already used a lot of the machine goes after one that hasn't.
- **Bids cost something.** An agent can pay for urgency (bid 1 to 10), but a bid of 10 spends twice
  the budget, so "always bid max" stops paying off. Budgets refill steadily, and unused allowance
  carries over for up to two days.
- **Agents know the rules without being told.** `firefighter setup` registers the MCP server with
  Claude Code, Codex, Gemini CLI and Cursor, and adds a short rule block to their global
  instructions. Every new session (including T3 Code threads built on them) queues heavy work, gives
  an honest time estimate, and tells you when it'll start.

## Install

**Windows** (PowerShell):

```powershell
irm https://raw.githubusercontent.com/JoshWright22/firefighter/main/scripts/install.ps1 | iex
```

**macOS / Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/JoshWright22/firefighter/main/scripts/install.sh | sh
```

That's it. The guard starts now and at every logon: Task Scheduler on Windows, launchd on macOS,
a systemd user unit on Linux. Uninstall with `firefighter uninstall`. Or build it yourself:
`go install github.com/JoshWright22/firefighter/cmd/firefighter@latest && firefighter setup`.

## What agents do (and what you can do too)

```sh
firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
firefighter run --minutes 5 -- npm run build                 # long CPU work
firefighter run --gpu --minutes 20 -- docker run --gpus all my/eval   # containers work too
firefighter run --detach --gpu --minutes 90 -- python sweep.py && firefighter wait <id>
firefighter status          # who's on stage until when, who's waiting, budgets
firefighter plan "big eval sweep" --when tonight --minutes 120 --gpu 90 --vram 12
firefighter extend <id> 15  # need more time than you said? say so
```

`run` waits for a turn, then runs the command **in the caller's own environment**: venv, conda, uv,
nvm and docker all just work. You get the command's normal output and exit code, then a receipt:

```
🔥 nanoGPT train [main set] waiting in the wings #2 in line, on ~14:05: GPU turn queue
🔥 nanoGPT train: spotlight's yours (lease 40 min, budget left 212.0)
...
🔥 nanoGPT train: curtain call, ok (exit 0) in 37.2 min (declared 40), waited 6.1, charged 31.4 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
```

MCP tools for agents: `firefighter_status`, `firefighter_submit`, `firefighter_wait`,
`firefighter_extend`, `firefighter_cancel`, `firefighter_plan`, `firefighter_config`.

## How it decides

Everything below is the default, with zero config. Each part can be swapped by name.

| Stage | Default | How it works | Swap with |
|---|---|---|---|
| **You** | dynamic reserve | Four states: away, present, active, game. The reserve grows within one tick (0.5 s) and is given back slowly: 1 CPU per 15 s, RAM after 60 s, VRAM after 120 s. Jobs are pinned to the CPUs outside your reserve. | `reserve.<state>.cpus / ram_pct / vram_gb / pause_gpu` |
| **Order** | `las`: least attained service | The project with the least decayed use (12 h half-life) goes next, Tiresias-style. A job waiting 45 min goes next no matter what. | `order.policy = las \| stride \| lottery \| drf \| fifo \| srpt` |
| **Price** | `priced` | A bid costs `bid/5` × usage and buys at most 30 min of queue lead. | `pricing.policy = priced \| free` |
| **Budget** | `karma` | Steady refill of `daily_minutes` (240), banked up to 2 days. An empty bucket drops the project to the spot band. | `budget.policy = karma \| daily \| none` |
| **Charge** | DRF dominant share | One machine-minute is all of the GPU, all of the VRAM, all of the RAM or all of the CPUs for one minute. Holding VRAM while idle is charged triple. | (built in) |
| **Admission** | learned peaks | A job's need is the larger of its declared size and 1.2 × its learned peak. It starts only if that fits after your reserve, and nobody jumps the head of the GPU line. | `gpu.max_jobs`, `gpu.pack_below_util`, `gpu.vram_margin` |
| **Preemption** | ladder | Shrink the job's CPU set → cooperative pause file → suspend (60 s max) → checkpoint file, then kill after 60 s grace (and requeue) → kill at once when RAM is critical. Spot jobs and jobs past their lease go first. | `ladder.*` |
| **Leases** | `--minutes` | Past 1.25 × the lease without `extend`, a job drops to spot and pays double. Runtime predictions use the median ratio of that command's actual to declared runtime. | `ladder.overrun_factor` |

Presets: `balanced` (default), `gamer`, `throughput`, `fair`. Change anything live:

```sh
firefighter config set preset gamer
firefighter config set order.policy drf
firefighter config set budget.projects.my-big-repo 600
firefighter config set reserve.exclusive_apps "[obs64.exe, blender.exe]"
firefighter config show
```

Agents can do the same through the `firefighter_config` MCP tool. The guard reloads within a second.

### Bands

| Band | Who | Preempted by |
|---|---|---|
| 0 · headliner | you, and everything not run by firefighter (apps, games, Docker, k8s) | nothing |
| 1 · opener | short CPU jobs (≤ 3 min) | you |
| 2 · main set | in-budget GPU and long jobs, the market | you; spot rules |
| 3 · afterparty | over budget, `--spot`, or past the lease | anything |
| 4 · harvester | optional idle-GPU program (Salad, BOINC, Folding@home): `harvester.start` / `harvester.stop` | anything |

### Making long GPU jobs pause politely

Windows has no way to checkpoint a CUDA process from outside the job, so the job itself has to
cooperate. `firefighter protocol` prints a 15-line helper. Call it once per training step: it pauses
while you game, and checkpoints and exits with code 75 when you need the memory back. firefighter
then puts the job back in line automatically.

## What it measures, per OS

| | Windows | macOS | Linux |
|---|---|---|---|
| Your input | GetLastInputInfo | IOHIDSystem idle time | GNOME idle monitor / xprintidle |
| Games and fullscreen | fullscreen foreground window, 3D-engine use per process (GPU Engine counters), exclusive apps | exclusive apps | exclusive apps |
| GPU and VRAM | nvidia-smi + per-process GPU counters (WDDM) | unified memory (RAM) | nvidia-smi, rocm-smi |
| Job priority | idle CPU, idle GPU scheduling, very low memory and I/O priority | nice 19 + background QoS | nice 19, SCHED_IDLE, idle I/O class |
| CPU reserve | affinity | QoS (no affinity on macOS) | affinity |
| Job tree | Job Object (no orphans), process tree | process group | process group |

## Research

The policy comes from a survey of about 40 repos and 30 papers: DRF, Tiresias, Themis, Gavel,
Karma, PerfIso, lottery and stride scheduling, BOINC, KAI-Scheduler, AgentLoom, and more. The full
report with sources is in [docs/research.md](docs/research.md).

## Testing

`go test ./...` runs the unit tests and an end-to-end suite. The suite builds the binary, starts a
real daemon, and scripts the human, memory pressure and a GPU through `FIREFIGHTER_FAKE_FILE`. It
covers GPU turns that never overlap, VRAM admission, game pause and resume, RAM-pressure checkpoint
and requeue, cancel, daemon restart, the MCP server, and setup/uninstall. CI runs it on Windows,
macOS (Intel and Apple silicon) and Linux (x64 and arm64) VMs, then tests both one-command installers.

MIT licensed.
