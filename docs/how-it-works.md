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
  requeue when you need the memory back, then kill, but only when RAM is critical, nothing is left
  to suspend, and the job's RAM is what's missing.
- **RAM nobody needs is reclaimed first.** On Windows the WSL2 VM (where Docker Desktop runs) keeps
  file cache from builds and installs, and Windows counts all of it as used. When RAM runs short
  FIREFIGHTER has the VM give that cache back before it touches any job, and says so in the log.
- **It learns what you and each command use.** Every run adds to its command's history for RAM,
  VRAM, CPUs, GPU load, disk writes and network, and the next run is admitted on that history, not
  on a guess. Your own use is recorded per state (away, present, active, game), and after a day
  your reserve follows it. `firefighter forecast` shows both.
- **Disk too.** It keeps free space on every drive jobs use (`reserve.disk_free_gb`): no job starts
  on a drive under the reserve, and a job writing to one is suspended until there's room again.
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
| **You** | dynamic reserve | Four states: away, present, active and game. The reserve grows within one 0.5 s tick and gives back 1 CPU every 15 s, RAM after 60 s and VRAM after 120 s. Jobs are pinned to the CPUs outside your reserve (Windows and Linux; macOS uses background QoS instead). While you're active or in a game the CPU reserve also follows what you and other programs use (that load + 2), up to `reserve.max_cpu_pct` (75%) of the CPUs: a Docker build on 13 of 16 CPUs leaves jobs 4, not 1. A state with `pause_gpu` (game, by default) pauses GPU jobs and starts no new ones. `reserve.disk_free_gb` (10, at most a tenth of the drive) is kept free on every drive jobs use. | `reserve.<state>.cpus / ram_pct / vram_gb / pause_gpu`, `reserve.max_cpu_pct`, `reserve.disk_free_gb`, `reserve.exclusive_apps` |
| **Order** | `las` | The project with the least use goes first, with a 12 h half-life (Tiresias, Slurm fair share). A 45-minute wait cap overrides it. | `order.policy = las \| stride \| lottery \| drf \| fifo \| srpt` |
| **Market** | `priced` | Price per machine-minute = tier (spot ⅓, normal 1, urgent 2) × congestion × 1.25 for projects whose jobs habitually overrun. Congestion is 0.5 × 8^u, where u is how committed the resource is over the next hour: 0.5 idle, about 1.4 at half load, 4 when full. The price is locked when the job is granted; overruns pay ×2 and adopted work ×1.5 on top. Urgent buys at most 30 min of queue lead under `las` and `stride`. | `pricing.policy = priced \| free`, `pricing.max_lead_minutes` |
| **Budget** | `karma` | A budget decides order and price, never whether spare capacity is used: an out-of-budget job is spot (after in-budget jobs, preempted first) and still starts whenever there is room. A project never owes more than one day of its allowance; a job demoted to spot mid-run pays the spot price from then on; a job the guard had to stop for your RAM is refunded. Steady refill of 240 machine-minutes a day (10 an hour), banked up to 2 days (Karma, OSDI '23). The first project on a machine starts full. Later new projects start at the median usage and draw their start and first day's refill from one pool all new projects share (120 a day), so inventing names doesn't mint budget or jump the queue. While a project's budget is empty, its waiting and running jobs are spot. | `budget.policy = karma \| daily \| none`, `budget.daily_minutes`, `budget.bank_days`, `budget.projects.<name>`, `budget.weights.<name>` |
| **Charge** | DRF | One machine-minute is all of the GPU, VRAM, RAM or CPUs for one minute (Dominant Resource Fairness). Idle VRAM is charged ×3 (ESX idle tax), overruns ×2, and chronic under-estimates ×1.25. | built in |
| **Learning** | on | Per command: decaying histograms of each run's peak RAM, peak VRAM and GB written, and of its mean CPUs, GPU load and network rate. Per user state: histograms of the CPUs you keep busy and of how far your RAM and VRAM use rises within 5 minutes. After a day of history in a state its reserve is the learned one, within half to three times the configured value. See [What it learns](#what-it-learns). | `reserve.learn` |
| **Admission** | learned peaks | A job's need is the larger of its declared size and 1.2 × its learned peak (from three runs on: the 98th percentile of its peaks + 15%, whatever it declares), checked against what's left on one GPU after your reserve and the unclaimed needs of jobs already there. Each card takes turns on its own (`gpu.max_jobs` per card, a second job only while that card's load leaves room), and a job goes to the least busy card that fits (fewest jobs, then lowest load, then most VRAM left); a need is clamped to what that card ever offers (its size, less your reserve, less the least others have held lately). The job is pinned to that card (`CUDA_VISIBLE_DEVICES` with PCI order, `HIP_VISIBLE_DEVICES`/`ROCR_VISIBLE_DEVICES`, or the oneAPI selector; `docker run --gpus` is narrowed too). First-time commands start 5 s apart so their real use shows. Short jobs (declared and predicted ≤ 10 min) may backfill behind a head that's waiting on VRAM or on RAM, so the GPU isn't idle while a big job waits. CPUs: a job whose past runs keep N CPUs busy waits while the jobs already running leave fewer than that outside your reserve (counted with a 25% allowance); a job with no history is never held for CPUs. GPU load: a job joins a busy card only if its usual load fits beside what the card carries. Disk: a job that usually writes N GB waits until its drive has that much beyond the disk reserve. RAM: a job starts only if its need plus `reserve.ram_margin_pct` (5% of RAM) fits beside your reserve, and not while RAM is, or within `reserve.ram_shrink_seconds` was, under the reserve; a GPU job with no `--ram` and no history is assumed to need `gpu.default_ram_gb` (3). A waiting job's line in `status` says what it lacks, and `status` leads with a note when the GPU is idle with jobs waiting. With no discrete GPU (Apple silicon), VRAM needs count as RAM. | `gpu.max_jobs` (per card), `gpu.pack_below_util`, `gpu.vram_margin`, `gpu.default_vram_gb`, `gpu.default_ram_gb`, `reserve.ram_margin_pct` |
| **Preemption** | ladder | See [the ladder](#the-preemption-ladder) below. | `ladder.*` |
| **Updates** | on | A released guard looks for a newer release 3 minutes after it starts and every 6 hours, and installs it at a moment when no job is running: it downloads the archive for this OS, checks it against the release's checksums, checks its signed build provenance when the GitHub CLI is signed in (a release without it is refused), runs the new program once, puts it in place of its own and restarts as the new version. Waiting jobs get back in line by themselves. If antivirus blocks the download it builds the same release from source when Go is installed. `firefighter update` does the same on request; a development build never replaces itself. | `update.auto` |
| **Guard** | on | Heavy processes an agent CLI (Claude Code, Codex, Gemini, Grok, OpenCode, Kimi, Copilot, Amp, Qwen, Aider, Goose, Cline and more) starts outside FIREFIGHTER get adopted: spot band, idle priority, charged. IDE terminals are never adopted. Each project is capped at 20 open jobs. Agents can read the policy but can't change it unless you allow it, and even then only `order.*`, `pricing.*` and `gpu.*` (never presets, which set reserves). | `guard.auto_adopt`, `guard.agent_parents`, `guard.heavy_*`, `guard.agents_can_configure` |

**Presets:** `balanced` (default), `gamer`, `throughput`, `fair` → `firefighter config set preset gamer`.

| Band | Who | Preempted by |
|---|---|---|
| 0 · headliner | you, plus everything not run by FIREFIGHTER (apps, games, Docker, k8s) | nothing |
| 1 · opener | short CPU jobs (≤ 3 min) | you |
| 2 · main set | in-budget GPU and long jobs (the market) | you |
| 3 · afterparty | over budget, `--spot`, past the lease, or adopted | anything |
| 4 · harvester | optional idle-GPU program (Salad, BOINC, Folding@home) | anything |

### Band 4: the harvester, and only while it pays

`[harvester] start`/`stop` are shell commands. FIREFIGHTER runs `start` after the GPU has been
idle `idle_seconds` (no GPU job queued or running, no game, no outside GPU work) and `stop` the
moment any of that changes. Options:

- `processes = ["vmmemWSL"]`: names whose GPU use is the harvester's own **while it runs**, so its
  load isn't mistaken for outside work. When it's off, the same names count as outside work again
  (the WSL VM also runs your own containers).
- `running = "<command>"`: exits 0 while the harvester's work runs. Checked every 2 minutes while
  FIREFIGHTER has it off; a harvester that started itself (its own auto-start) is stopped again.
- `[harvester.profit]`: the profit guard. Set `cents_per_kwh` (all-in) to turn it on, and add
  `[[harvester.profit.periods]]` (`months`, `days`, `from`, `to`, `cents`) for time-of-use prices.
  While it runs, its power is priced into `harvest-ledger.csv`, one row per hour:
  wall watts = (GPU board power from nvidia-smi + CPUs busy above the pre-start baseline ×
  `cpu_watts` + `extra_watts`) / `psu_efficiency`. Before each start and every minute after, the
  expected earnings per hour (`earn_per_hour` less `cut_pct`, replaced by the rate from reported
  earnings once they cover `min_hours`) must beat the power cost at this hour's price, so hours
  that would lose money are never harvested. Earnings go into `harvest-earnings.csv`
  (`firefighter harvest earned 3.20`). If the trailing `trailing_days` net (earned less the cut,
  less the power cost of the hours those reports cover) goes negative, it trips off and stays off
  (`harvest-trip.txt`) until `firefighter harvest reset`. Harvested hours with no earnings report
  for `stale_days` hold it off too. `firefighter harvest` prints the books and the next 24 h by price.

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
4. **Checkpoint:** when VRAM (per card) stays under your reserve for 20 s, or when a spot job
   has run its 10-minute minimum and an in-budget job is waiting on what it holds, FIREFIGHTER lifts
   any pause and creates `FIREFIGHTER_CHECKPOINT_FILE`. The job gets 60 s to save and exit. On Unix it then gets a
   SIGTERM, and 10 s later it's killed. A job that exits with 75, or exits at all after the
   request, is put back in line under the same id. `--evict force` jobs are killed at once (and put back in line the same way).

Victims are picked so one eviction fixes the problem: first jobs whose memory covers the shortfall,
then the highest band (spot, past their lease, adopted), then the heaviest. One eviction at a time.
CUDA can't be checkpointed from outside on Windows or macOS, so the job has to cooperate.
`firefighter protocol` prints a short Python helper you call once per step.

## What it learns

FIREFIGHTER's first versions asked agents how much a job needs and kept a fixed share of the
machine for you. Both are guesses. Now it measures, and the measurements decide. The data
structure and the rules are the ones Google's Autopilot and the Kubernetes Vertical Pod
Autoscaler use to size jobs, cut down to one PC. The math is in `internal/usage`, the sources are
in [research.md](research.md#learning-usage), and `firefighter forecast` prints what has been
learned.

**Each command** (the same command line in the same project) keeps one sample per finished run in
a histogram whose buckets are 5% wide, where older runs fade:

| Resource | Sample per run | Fades by half every | Next run is admitted on |
|---|---|---|---|
| RAM, VRAM | the peak | 8 runs | no history: what it declares, else what the project's other commands of its kind need (their median), else `gpu.default_ram_gb` / `gpu.default_vram_gb`. 1-2 runs: the larger of the declaration and the highest peak × 1.2. 3+ runs: the 98th percentile of the peaks × 1.15; a declaration above anything seen is cut to 1.5 × the highest peak |
| Disk | GB written | 8 runs | the same rule, against the free space beyond the disk reserve |
| CPUs, GPU load, network | the mean over the run | 2 runs | the weighted mean (jobs can be slowed on these without losing work, so the mean is the right size) |

A run that was stopped for lack of RAM records 1.2 × the peak it reached (or +100 MiB), because
its real need is above that. No need is ever counted above what the machine can give a job: its
RAM, less what is always in use outside jobs (the least seen lately), less the reserve while you're
away and the admission margin. A job whose history points higher is counted at that ceiling, starts
when the machine is at its emptiest, and says so in `status`.

**You** are everything that isn't a FIREFIGHTER job. Every 5 s, for the state you're in:

| Resource | Sample | Your reserve in that state, after a day of history |
|---|---|---|
| CPUs | CPUs busy outside jobs (not counted while every CPU is busy: that shows what you got, not what you wanted) | the level that covers 95% of your work (not of your time: one minute of compiling outweighs nine of reading), + 2 idle cores |
| RAM, VRAM | how far your use rises within the next 5 minutes (the WSL/Docker VM's memory is left out: its cache is reclaimed, not reserved for) | the 95th percentile of that rise + 15%: the free memory that has to be there now |
| Network | Mbps while no network job runs | shown in `forecast`; network jobs still wait while you use over `network.human_busy_pct` of the link |

History fades with a 7-day half-life. The learned reserve stays between half and three times the
configured one, and the instant rules stay on top: the CPU reserve still follows what is in use
this second, and a game still holds the GPU. `reserve.learn false` goes back to the configured
values.

### When RAM runs short

**The floor.** Free RAM never planned below `reserve.min_free_gb` (4 GB, at most 15% of RAM), even
when the learned reserve is smaller: under it Windows pages and the desktop stalls. A job starts
only when its full need plus the margin fits above that floor, and on Windows only when the commit
left (RAM + pagefile promised to programs) covers its need plus `reserve.commit_free_gb` (3 GB). A
container's `docker run --memory` counts: its reservation is the declared or learned need plus
half, never above the limit (the whole limit when nothing is known), and once it runs the
container is held to that reservation with `docker update`, so it can't grow into paging. When RAM
is critical and the WSL VM holds 2 GB or more, its cache is reclaimed even if its own numbers
aren't in or the last reclaim returned little.

A kill throws away everything a job has computed since it last wrote a file, so RAM has its own
order, and each step runs only if the one before didn't fix it:

1. **Reclaim what nobody needs.** On Windows the WSL2 VM (Docker Desktop's engine runs in it) fills
   with file cache during a build or an install; Windows sees one process, `vmmemWSL`, holding all
   of it. FIREFIGHTER reads the VM's own numbers once RAM gets tight (`status` shows "holds 19.0 GB,
   uses 0.9 GB"), and as soon as RAM is under your reserve, or a waiting job lacks only RAM, it has
   the VM drop its page cache and compact its memory, which hands the pages back to Windows in
   seconds without stopping anything inside. No job is touched while that runs. The log says what
   was asked and what came back. Off with `reserve.reclaim_vm_cache false`.
2. **Suspend, don't stop.** If RAM stays under your reserve for 20 s (or drops under 3%: at once),
   one job at a time is suspended, the one whose RAM covers the shortfall first. On Windows its
   memory is moved out to the pagefile, so suspending really frees it; the VRAM stays where it is.
   It resumes where it stopped once RAM has room for what it takes back plus the margin, and its
   receipt says how long it was held and how much RAM that gave back. Held time is not lease time
   and is not charged. On macOS and Linux a stopped process keeps its RAM, so there the job that
   covers the shortfall is asked to checkpoint and exit instead (step 4 above).
3. **Stop, last.** Only when RAM stays under 3%, nothing is left to suspend (or suspending freed
   nothing: locked memory, containers), and the jobs' RAM is what's missing. A job whose RAM would
   not cover the shortfall, alone or with the other jobs, is never stopped for it: the log says the
   jobs aren't the cause. The reason, the free RAM and the RAM the stop freed go in the log and the
   receipt.

Admission is the other half: a job is not granted into a shortage (see Admission above).

### When a drive fills up

`reserve.disk_free_gb` (10 GB, at most a tenth of the drive; 0 turns it off) is kept free on the
drive of each job's folder and on the drives temp files and FIREFIGHTER's own state live on. No job
starts on a drive under it. If a drive stays under it for 20 s, the running job writing the most to
it is suspended (stopping it would free nothing, and its files stay), and resumes when the drive has
room again. Disk I/O is handled by priority: on Windows and Linux jobs run at the lowest I/O
priority, so your own reads and writes go first.


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
| Idle-core buffer protects latency, while priority and CPU caps don't | PerfIso (ATC '18) | The human's CPU reserve = max(state base, CPUs the human and other programs are using + 2, up to `reserve.max_cpu_pct` of the machine) in the active and game states. The load it follows is the least of the last three readings (1.5 s), measured per job process, so a job starting or a worker exiting doesn't read as your demand. Jobs are pinned to the CPUs outside it. Grows within one tick, gives back 1 CPU per 15 s. | `policy.ReserveCtl`, `daemon.enforceCPU` |
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
| Learned memory peaks beat estimators | GPUMemNet, LLMem, FakeTensor study | VRAM and RAM need = max(declared, 1.2 × learned peak) for the first runs, then the 98th percentile of the peaks + 15%. A need the card can never free is clamped. | `daemon.fillNeeds`, `usage.PeakNeed` |
| Decaying histograms; peak for memory, mean for CPU | Autopilot (EuroSys '20, §3.2, Eq. 1-3); Kubernetes VPA | 5%-wide buckets, weight 2^(−age/half-life). Memory and VRAM: P98 of per-run peaks + 15%. CPUs, GPU load, network: the decayed mean. Half-lives 8 runs and 2 runs (Autopilot's 48 h and 12 h, 4:1). | `usage.Hist`, `usage.PeakNeed`, `usage.RateNeed` |
| Load-adjusted percentile for bursty use | Autopilot Eq. 3 | The human's CPU reserve is the P95 of their load weighted by the load itself, + 2 idle cores (PerfIso). | `usage.Hist.LoadPercentile`, `daemon.learnedReserve` |
| A running job's unused request is given back | Borg reservations (EuroSys '15, §5.5): the request for 300 s, then a slow decay towards usage plus a margin | The RAM held back for a running job beyond what it uses halves every 10 minutes of work after the first 5, down to its own peak + 15%. | `daemon.reservedRAM` |
| Count a young job at its declared limit | Take it to the limit (EuroSys '21, §4) | Under 3 runs the declaration is a floor; from 3 runs history decides. | `usage.PeakNeed` |
| Bump after an out-of-memory kill | VPA `RecordOOM` (×1.2, +100 MiB) | A run stopped for RAM records max(peak × 1.2, peak + 100 MiB). | `usage.OOMBump`, `daemon.learnRun` |
| Predict from the nearest level with history | 3Sigma (EuroSys '18, §4.1) | A never-seen command starts from the median of its project's commands of the same kind. | `daemon.projectPrior` |
| Oversubscribe what can be throttled | Resource Central (SOSP '17, Alg. 1: 125%) | Jobs' learned mean CPUs may add up to 125% of the CPUs outside the human's reserve. | `daemon.cpuRoom`, `daemon.cpuWhy` |
| Pack a GPU only when loads fit | Gandiva (OSDI '18, §5) | A job with a learned GPU load joins a busy card only if the sum stays under 110%. | `daemon.admit` |
| Don't learn from saturated samples | SmartHarvest (EuroSys '21) | The human's CPU use isn't recorded while every CPU is busy. | `daemon.observeHuman` |
| Little history, little trust | VPA confidence (`getConfidence`) | A state's reserve is learned only after a day of samples in that state, and stays within 0.5-3× the configured value. | `usage.Reserve` |
| EASY backfill | Lifka; Slurm | Behind a GPU head that's waiting on VRAM, only jobs ≤ 10 predicted minutes may start. | `daemon.admit` |
| Minimum guaranteed runtime | KAI-Scheduler | A spot GPU job yields to an in-budget one only after `min_runtime_minutes` (10), and only when the waiting job is blocked by the GPU alone. | `daemon.ladder` |
| Priority bands above the market | Borg | Band 0 (the human) is never in the market. Then 1 short, 2 batch, 3 spot, 4 harvester. | `policy.BandFor` |
| Step-boundary suspend | Gandiva | Cooperative pause file; suspend only if not acknowledged in 10 s. | `daemon.ladder` |
| Checkpoint on eviction | CheckFreq, Borg eviction notice | Checkpoint file plus 60 s grace, then SIGTERM (Unix), then kill. Exit 75 or any exit after the request requeues the job. | `daemon.requestCheckpoint`, CLI `run` |
| Kill order under memory pressure | Android lmkd, earlyoom | Highest band first, then heaviest; evict=never jobs last. For RAM a kill is the last step: reclaim, then suspend, then stop only a job whose RAM covers the shortfall. | `daemon.victim`, `daemon.ramPressure` |
| Reclaimable memory isn't demand | Linux MemAvailable; WSL2 free page reporting; balloon drivers (Waldspurger, ESX) | The WSL2/Docker VM's page cache is read from the guest and given back to Windows (drop caches + compaction) before any job is touched. | `sysinfo.VMGuest`, `daemon.reclaimVM` |
| Suspend to swap instead of kill | Windows working-set trimming; Borg's "evict, don't lose work" | A job suspended for RAM has its working set emptied to the pagefile and resumes later. | `sysinfo.Trim`, `daemon.hold` |
| Harvest idle GPU | PilotFish, Salad, BOINC | Band 4 harvester runs only after 180 s with no GPU jobs and no game. | `daemon.harvester` |
| One broker, agent protocol | AgentLoom, Anvil, gpubroker | MCP tools, `wait`, receipts, leases, evict policy, `docker run` labelling. | `internal/mcp`, CLI |
| Memory, not CPU, limits concurrent agents | AgentCgroup (2026) | RAM headroom gates every admission, with a margin, and never into a shortage; the RAM reserve is part of the human's reserve. | `daemon.headroom`, `daemon.admit` |
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
