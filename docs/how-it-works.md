# How FIREFIGHTER works

This page covers what FIREFIGHTER does, how it decides (and how to change it), the preemption
ladder, and where each idea from the research (DRF, Tiresias, Karma and the rest) lives in the
code, with the numbers the code actually uses. If this page and the code disagree, the code wins:
`internal/policy` holds the math, `internal/daemon` holds the loop.

## What it does, in detail

- **You always come first.** It watches your keyboard and mouse, fullscreen apps, games (3D-engine
  use), and every process it didn't start: Docker containers, Kubernetes pods, local model servers,
  your own apps. It keeps a **dynamic reserve** of CPUs, RAM and VRAM for you that grows within half a
  second when you sit down or start a game and gives capacity back slowly when you leave. VRAM that
  runs out shows up as CUDA out-of-memory, or a silent spill into system RAM on Windows; the reserve
  and VRAM admission are there to prevent both.
- **Agents take turns instead of fighting.** Heavy work waits in a queue. A GPU job starts only
  when its VRAM need fits, and that need is learned from the command's past runs. Projects that have
  used the least go first (least attained service), with a 45-minute cap on any wait.
- **A market maker, not a bidding war.** Agents don't bid against each other. FIREFIGHTER posts
  prices for three tiers: **spot** (leftovers, ⅓ price), **normal**, and **urgent** (2×, only
  when you're waiting on the result). Every reply an agent already gets ends with one line: when
  each tier would start, what it costs against the agent's budget, the odds the GPU is busy over
  the next 2 hours, a quiet hour to aim for, and a `do:` with the one recommended action. The
  advice never tells an agent to outbid anyone, so the whole market costs about 60 tokens and
  there's no bidding war to win.
- **Fallbacks for careless or rogue agents.** Heavy processes an agent starts outside FIREFIGHTER
  are found and adopted anyway: idle priority, billed 1.5×, preempted first. That makes skipping
  the queue strictly worse than using it. Other guards cover queue flooding (per project and in
  total), new project names that try to get a fresh budget (all new projects share one small
  daily pool), chronic under-estimates (×1.25), VRAM hoarding (×3), lease overruns (×2, preempted
  first), leases over 24 h, extensions after a short backfill, and one agent cancelling or
  re-bidding another agent session's job (you, from a terminal or editor, can manage any job).
  Text an agent sends (plans, names) is stripped of control characters before other agents or
  your terminal see it.
- **Graceful for the agent.** When you need the machine back, the job's own client says why,
  and the job gets the whole grace period (60 s) to save and exit (checkpoint protocol). On
  macOS/Linux a job still running 10 s before the deadline gets SIGINT (Ctrl+C) before SIGTERM; on
  Windows it keeps the full grace period, then is stopped. It's requeued under the same id and the
  agent is told why and that there's nothing to resubmit.
- **Polite preemption.** The ladder goes: shrink the job's CPU set, then cooperative pause at a step
  boundary, then suspend (for jobs that ignore the pause), then checkpoint-and-exit with automatic
  requeue when you need the memory back, then kill, but only when RAM is critical.
- **Network too.** Downloads and uploads (`docker pull`, pip/npm/cargo installs, `git clone`,
  model and dataset downloads; detected automatically, or `--net`) take turns, wait while you're
  using the link (a call, a stream: `network.human_busy_pct`), and pause while you game.
- **Any machine.** Any number of GPUs from any vendor (NVIDIA, AMD, Intel; mixed is fine), any
  CPU count (Windows processor groups past 64 included), any RAM size. Each GPU job is placed on the
  card it was admitted on, in that vendor's own numbering. On mixed machines a job goes to the
  NVIDIA cards unless it says otherwise (`--gpu-vendor amd|intel|any`). Every PC runs its own
  FIREFIGHTER; nothing goes over the network.
- **Set and forget.** `setup` starts the guard at logon and, for every agent it finds installed,
  registers the MCP server and adds a short rule block to its global instructions, so every new
  session (including T3 Code threads) already knows the protocol: Claude Code, Codex, Gemini CLI,
  Antigravity, Cursor, Grok Build, OpenCode, Kimi CLI and Kimi Code, Pi, Muse Code, fx, Kiro,
  Windsurf/Devin, Copilot CLI, Amp, Qwen Code, Crush, Cline, Goose and Droid. Each in its own
  format; `uninstall` removes all of it.

## How it decides (and how to change it)

Zero config gives you the researched defaults below. You can swap each part by name, live, with
`firefighter config set <key> <value>`. Agents read the policy with `firefighter config show`; the
`firefighter_config` MCP tool (to change it) is listed only if you set `guard.agents_can_configure true`.

| Stage | Default | What it does | Swap with |
|---|---|---|---|
| **You** | dynamic reserve | Four states: away, present, active and game. The reserve grows within one 0.5 s tick and gives back 1 CPU every 15 s, RAM after 60 s and VRAM after 120 s. Jobs are pinned to the CPUs outside your reserve (Windows and Linux; macOS uses background QoS instead). A state with `pause_gpu` (game, by default) pauses GPU jobs and starts no new ones. | `reserve.<state>.cpus / ram_pct / vram_gb / pause_gpu`, `reserve.exclusive_apps` |
| **Order** | `las` | The project with the least use goes first, with a 12 h half-life (Tiresias, Slurm fair share). A 45-minute wait cap overrides it. | `order.policy = las \| stride \| lottery \| drf \| fifo \| srpt` |
| **Market** | `priced` | Price per machine-minute = tier (spot ⅓, normal 1, urgent 2) × congestion × 1.25 for projects whose jobs habitually overrun. Congestion is 0.5 × 8^u, where u is how committed the resource is over the next hour: 0.5 idle, about 1.4 at half load, 4 when full. The price is locked when the job is granted; overruns pay ×2 and adopted work ×1.5 on top. Urgent buys at most 30 min of queue lead under `las` and `stride`. | `pricing.policy = priced \| free`, `pricing.max_lead_minutes` |
| **Budget** | `karma` | Steady refill of 240 machine-minutes a day (10 an hour), banked up to 2 days (Karma, OSDI '23). The first project on a machine starts full. Later new projects start at the median usage and draw their start and first day's refill from one pool all new projects share (120 a day), so inventing names doesn't mint budget or jump the queue. While a project's budget is empty, its waiting and running jobs are spot. | `budget.policy = karma \| daily \| none`, `budget.daily_minutes`, `budget.bank_days`, `budget.projects.<name>`, `budget.weights.<name>` |
| **Charge** | DRF | One machine-minute is all of the GPU, VRAM, RAM or CPUs for one minute (Dominant Resource Fairness). Idle VRAM is charged ×3 (ESX idle tax), overruns ×2, and chronic under-estimates ×1.25. | built in |
| **Admission** | learned peaks | A job's need is the larger of its declared size and 1.2 × its learned peak, checked against what's left on one GPU after your reserve and the unclaimed needs of jobs already there. Each card takes turns on its own (`gpu.max_jobs` per card, a second job only while that card's load leaves room), and a job goes to the least busy card that fits (fewest jobs, then lowest load, then most VRAM left); a need is clamped to what that card ever offers (its size, less your reserve, less the least others have held lately). The job is pinned to that card (`CUDA_VISIBLE_DEVICES` with PCI order, `HIP_VISIBLE_DEVICES`/`ROCR_VISIBLE_DEVICES`, or the oneAPI selector; `docker run --gpus` is narrowed too). First-time commands start 5 s apart so their real use shows. Short jobs (declared and predicted ≤ 10 min) may backfill behind a head that's waiting on VRAM. With no discrete GPU (Apple silicon), VRAM needs count as RAM. | `gpu.max_jobs` (per card), `gpu.pack_below_util`, `gpu.vram_margin`, `gpu.default_vram_gb` |
| **Preemption** | ladder | See [the ladder](#the-preemption-ladder) below. | `ladder.*` |
| **Guard** | on | Heavy processes an agent CLI (Claude Code, Codex, Gemini, Grok, OpenCode, Kimi, Copilot, Amp, Qwen, Aider, Goose, Cline and more) starts outside FIREFIGHTER get adopted: spot band, idle priority, charged. IDE terminals are never adopted. Each project is capped at 20 open jobs. Agents can read the policy but can't change it unless you allow it, and even then only `order.*`, `pricing.*` and `gpu.*` (never presets, which set reserves). | `guard.auto_adopt`, `guard.agent_parents`, `guard.heavy_*`, `guard.agents_can_configure` |

**Presets:** `balanced` (default), `gamer`, `throughput`, `fair` → `firefighter config set preset gamer`.

| Band | Who | Preempted by |
|---|---|---|
| 0 · headliner | you, plus everything not run by FIREFIGHTER (apps, games, Docker, k8s) | nothing |
| 1 · opener | short CPU jobs (≤ 3 min) | you |
| 2 · main set | in-budget GPU and long jobs (the market) | you |
| 3 · afterparty | over budget, `--spot`, past the lease, or adopted | anything |
| 4 · harvester | optional idle-GPU program (Salad, BOINC, Folding@home) | anything |

## The preemption ladder

Steps run in order, and each one fires only if the trigger is still there:

1. **CPU set.** Jobs always live outside your CPU reserve (Windows, Linux).
2. **Cooperative pause** (GPU jobs, while your state holds the GPU): FIREFIGHTER creates the file
   at `FIREFIGHTER_PAUSE_FILE`. A job that honors it creates `FIREFIGHTER_PAUSED_FILE` and sleeps
   until the pause file is gone.
3. **Suspend:** if the job hasn't acknowledged within 10 s (or, where per-process GPU counters
   exist, acknowledged but kept computing), it's suspended. It stays suspended while you need the
   GPU. Past 60 s, if the memory it holds is what you're short of, it's stopped and requeued (it
   ignored the pause, so it can't checkpoint). `--evict never` jobs stay suspended until you're done.
4. **Checkpoint:** when RAM or VRAM (per card) stays under your reserve for 20 s, or when a spot job
   has run its 10-minute minimum and an in-budget job is waiting on what it holds, FIREFIGHTER lifts
   any pause and creates `FIREFIGHTER_CHECKPOINT_FILE`. The job gets 60 s to save and exit. On Unix it then gets a
   SIGTERM, and 10 s later it's killed. A job that exits with 75, or exits at all after the
   request, is put back in line under the same id. `--evict force` jobs are killed at once (and put back in line the same way).
5. **Kill:** if available RAM drops under 3%, one job is killed at once, even mid-grace, and the
   next only after 5 s (memory takes a moment to come back).

Victims are picked so one eviction fixes the problem: first jobs whose memory covers the shortfall,
then the highest band (spot, past their lease, adopted), then the heaviest. One eviction at a time.
`--evict never` jobs are only ever touched when RAM is critical. CUDA can't be checkpointed from outside on Windows
or macOS, so the job has to cooperate. `firefighter protocol` prints a short Python helper you
call once per step.


## The loop

Twice a second the daemon:

1. Reads the machine: CPU, RAM, every GPU's VRAM and use, per-process GPU use where the OS gives
   it, how long since your last input, and any fullscreen foreground app.
2. Measures every job's process tree. The slow scans run outside the lock. Each process is
   identified by its pid **plus its start time**, so a reused pid is never touched.
3. Works out your state and sizes your reserve.
4. Pins jobs to the CPUs outside the reserve and walks the preemption ladder.
5. Closes jobs whose client is gone, re-places waiting jobs whose project's budget changed, and
   admits waiting jobs in policy order.

## Paper → implementation map

| Idea | Source | In FIREFIGHTER | Where |
|---|---|---|---|
| Idle-core buffer protects latency, while priority and CPU caps don't | PerfIso (ATC '18) | The human's CPU reserve = max(state base, CPUs the human and other programs are using + 2) in the active and game states. Jobs are pinned to the CPUs outside it. Grows within one tick, gives back 1 CPU per 15 s. | `policy.ReserveCtl`, `daemon.enforceCPU` |
| Fast attack, slow decay; hold before giving back | PerfIso, Heracles, BOINC (15–20 s) | The RAM reserve holds 60 s and the VRAM reserve 120 s before shrinking. Game state holds 30 s after the game ends. | `ReserveCtl.Update`, `reserve.*_seconds` |
| "User active" = input in the last 3 minutes; GPU work yields to the user | BOINC | `away_seconds = 180`; the game state pauses GPU jobs (`pause_gpu`, any state can set it). | `policy.UserState`, `daemon.ladder` |
| Exclusive apps | BOINC `exclusive_gpu_apps` | `reserve.exclusive_apps` counts as the game state. | `daemon.senseUser` |
| Least attained service when job lengths are unknown | Tiresias (NSDI '19) | `order.policy = las`: decayed machine-minutes per weight, lowest first. | `policy.Key` |
| Fair share decays over time | Slurm fair share | 12 h half-life. | `Account.Decay` |
| Multi-resource fairness | DRF (NSDI '11) | A job is charged its dominant share each minute: max(GPU use, VRAM/card, RAM/total, CPUs/total). `order.policy = drf` orders by the decayed dominant share. | `Account.Charge`, `policy.Key` |
| Proportional share | Lottery, stride (OSDI '94) | `order.policy = lottery` draws by weight × bid price; `stride` uses a pass value that never decays. | `policy.Order` |
| Budgets that reward not grabbing | Karma (OSDI '23) | A steady refill of `daily_minutes`, banked up to `bank_days`. An empty budget puts the project's waiting jobs in the spot band. | `Account.Refill`, `daemon.refreshBands` |
| Pay per use instead of free bids | Tycoon, Spawn | The posted price at grant = tier (spot ⅓, normal 1, urgent 2) × congestion × surcharge, locked for the job's run. | `policy.Price`, `daemon.lockPrice` |
| Online posted prices that rise with commitment | Zhang, Huang & Wu (SIGMETRICS '17), primal-dual pricing | Congestion = 0.5 × 8^u, where u = committed minutes in the next hour ÷ slot-minutes. | `policy.Congestion` |
| Posted prices, not auctions, for LLM bidders | InfoBid (2025), Survival Games | Three tiers instead of a 1–10 bid. Quotes show aggregate data (start, cost, busy odds), never rival bids, and the advice never recommends outbidding. Taking a posted price is the dominant strategy. | `policy/market.go`, `daemon/market.go` |
| Avoid herding into the same "free" hour | El Farol / congestion-game information design | Each job gets its own quiet-hour suggestion: a hash of its id picks among the near-quietest hours, skipping announced plans and hours with under 3 h of history. | `policy.QuietWindow` |
| Mean residual life grows with age (heavy tails) | Harchol-Balter & Downey; SOAP / Gittins | Time left for a running job: before its prediction, the prediction minus its age; past it, half its age again. | `policy.Remaining` |
| Hard bound on waiting | Starvation freedom | Anything waiting `max_wait_minutes` (45) goes next within its band. | `policy.Order` |
| User runtime estimates are biased | Tsafrir, Etsion & Feitelson | Prediction = declared × median of the last 5 actual/declared ratios for this command. Projects whose median is over 1.5 pay ×1.25. | `policy.Predict`, `policy.Surcharge` |
| Leases | Themis, AgentLoom `gpu_extend` | Past 1.25 × the lease (paused time excluded) without `extend`, a job drops to spot and pays ×2. | `daemon.ladder` |
| Idle memory tax | Waldspurger, ESX | VRAM held while GPU use is under 15% is charged ×3. | `daemon.applyMeasurements` |
| Pack only on sustained headroom | Gandiva (OSDI '18) | A second GPU job joins only while the ~30 s smoothed utilization is under `pack_below_util`. | `daemon.admit` |
| Learned memory peaks beat estimators | GPUMemNet, LLMem, FakeTensor study | VRAM and RAM need = max(declared, 1.2 × learned peak). The learned peak decays 10% per run. A need the card can never free is clamped. | `daemon.fillNeeds` |
| EASY backfill | Lifka; Slurm | Behind a GPU head that's waiting on VRAM, only jobs ≤ 10 predicted minutes may start. | `daemon.admit` |
| Minimum guaranteed runtime | KAI-Scheduler | A spot GPU job yields to an in-budget one only after `min_runtime_minutes` (10), and only when the waiting job is blocked by the GPU alone. | `daemon.ladder` |
| Priority bands above the market | Borg | Band 0 (the human) is never in the market. Then 1 short, 2 batch, 3 spot, 4 harvester. | `policy.BandFor` |
| Step-boundary suspend | Gandiva | Cooperative pause file; suspend only if not acknowledged in 10 s. | `daemon.ladder` |
| Checkpoint on eviction | CheckFreq, Borg eviction notice | Checkpoint file plus 60 s grace, then SIGTERM (Unix), then kill. Exit 75 or any exit after the request requeues the job. | `daemon.requestCheckpoint`, CLI `run` |
| Kill order under memory pressure | Android lmkd, earlyoom | Highest band first, then heaviest; evict=never jobs last. Critical RAM (3%) kills at once, even mid-grace. | `daemon.victim` |
| Harvest idle GPU | PilotFish, Salad, BOINC | Band 4 harvester runs only after 180 s with no GPU jobs and no game. | `daemon.harvester` |
| One broker, agent protocol | AgentLoom, Anvil, gpubroker | MCP tools, `wait`, receipts, leases, evict policy, `docker run` labelling. | `internal/mcp`, CLI |
| Memory, not CPU, limits concurrent agents | AgentCgroup (2026) | RAM headroom gates every admission; the RAM reserve is part of the human's reserve. | `daemon.headroom` |
| Simple coordination captures most of the gain | AIOS | The defaults are simple and swappable. | `internal/config` |

## Not implemented, and why

| Idea | Why not |
|---|---|
| Spatial GPU sharing with per-process VRAM caps (MPS, MIG, HAMi) | MPS is Linux/QNX only, MIG isn't on consumer cards, and HAMi needs `LD_PRELOAD` on Linux. FIREFIGHTER packs by learned peaks instead (`gpu.max_jobs`). |
| Transparent CUDA checkpointing (cuda-checkpoint, CRIUgpu) | Linux only. The cooperative checkpoint file works on every OS. |
| Kernel-level preemption (REEF, TGS, Orion) | Needs driver or framework hooks. WDDM's own time-slicing plus idle GPU priority covers the desktop case. |
| PresentMon frame-time feedback for the reserve | Planned. Today the reserve follows input, fullscreen apps and 3D-engine use. |
| VCG / combinatorial auctions | Overkill for one machine, and LLM bidders game visible auctions. Posted prices give the same information without the game. |
| Gavel's LP-based heterogeneity policies | Not needed at one-PC scale. Each GPU job is admitted on the freest card (after the unclaimed needs of jobs already there) and pinned to it in its vendor's numbering; VRAM pressure is checked per card. |

## Market maker details

Every quote, for the kind of job asked about (GPU or CPU):

- **Congestion**: 0.5 × 8^u, where u = committed minutes of that resource in the next hour ÷
  slot-minutes, capped at 1. Idle is half price to encourage filling the machine; a fully
  committed GPU is 4×.
- **Menu**: spot, normal and urgent. Each tier's queue position comes from the real ordering
  policy, its start time from one schedule simulation (running leases with heavy-tail remaining
  time, the human holding the GPU, GPU work outside FIREFIGHTER), and its cost = predicted
  minutes × dominant share × tier price × congestion × surcharge.
- **Busy odds** at 15/30/60/120 min: P = 1 − (1 − P_sched)(1 − w(h)·P_hist)(1 − P_plan).
  P_sched is 0.95 when the simulation has every slot full, falling as the horizon grows.
  w(h) = 1 − e^(−h/60) weighs in the hour-of-week history once that hour has 3+ hours of
  observations; it moves a quarter of the way per visit, so it remembers several weeks. P_plan
  is 0.7 when an announced GPU plan covers that time.
- **do:** never "outbid". Short budget: spot or wait for the refill. Urgent tier: keep it only
  if the human is waiting. A start over 60 min away: queue it, or spot, or plan it for the quiet
  hour. Otherwise: queue at normal and keep working.
- **One estimate everywhere**: `status`, `wait`, the CLI waiting line, MCP and the menu all
  read the same simulation.

Agents see a one-line reply (about 60 tokens). The full numbers are in `status --json`, the
quote JSON and MCP `structuredContent`.

## Research

The policy comes from a survey of about 40 repos and 30 papers. Among them: DRF (NSDI '11),
Tiresias (NSDI '19), Themis (NSDI '20), Gavel (OSDI '20), Karma (OSDI '23), PerfIso (ATC '18),
PilotFish (ATC '22), lottery and stride scheduling (OSDI '94), BOINC, KAI-Scheduler, AgentCgroup
and AIOS. The map above ties each idea to the code that uses it. The full report with sources is
in [research.md](research.md).
