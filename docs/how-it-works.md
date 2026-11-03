# How firefighter works: from the research to the code

This page maps each idea from the research (DRF, Tiresias, Karma and the rest) to where the code
uses it, with the numbers the code actually uses. If this page and the code disagree, the code
wins: `internal/policy` holds the math, `internal/daemon` holds the loop.

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

| Idea | Source | In firefighter | Where |
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
| Pay per use instead of free bids | Tycoon, Spawn | The posted price at grant = bid/5 × congestion (0.5–4) × surcharge, locked for the job's run. | `policy.Price`, `daemon.lockPrice` |
| Posted prices, not auctions, for LLM bidders | InfoBid (2025), Survival Games | Quotes show aggregate market data (price, menu, busy odds), never rival bids. Accepting a posted price is the dominant strategy. | `policy/market.go`, `daemon/market.go` |
| Hard bound on waiting | Starvation freedom | Anything waiting `max_wait_minutes` (45) goes next within its band. | `policy.Order` |
| User runtime estimates are biased | Tsafrir, Etsion & Feitelson | Prediction = declared × median of the last 5 actual/declared ratios for this command. Projects whose median is over 1.5 pay ×1.25. | `policy.Predict`, `policy.Surcharge` |
| Leases | Themis, AgentLoom `gpu_extend` | Past 1.25 × the lease (paused time excluded) without `extend`, a job drops to spot and pays ×2. | `daemon.ladder` |
| Idle memory tax | Waldspurger, ESX | VRAM held while GPU use is under 15% is charged ×3. | `daemon.applyMeasurements` |
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
| Spatial GPU sharing with per-process VRAM caps (MPS, MIG, HAMi) | MPS is Linux/QNX only, MIG isn't on consumer cards, and HAMi needs `LD_PRELOAD` on Linux. firefighter packs by learned peaks instead (`gpu.max_jobs`). |
| Transparent CUDA checkpointing (cuda-checkpoint, CRIUgpu) | Linux only. The cooperative checkpoint file works on every OS. |
| Kernel-level preemption (REEF, TGS, Orion) | Needs driver or framework hooks. WDDM's own time-slicing plus idle GPU priority covers the desktop case. |
| PresentMon frame-time feedback for the reserve | Planned. Today the reserve follows input, fullscreen apps and 3D-engine use. |
| VCG / combinatorial auctions | Overkill for one machine, and LLM bidders game visible auctions. Posted prices give the same information without the game. |
| Gavel's LP-based heterogeneity policies | Nothing to optimize across GPU types on one PC. Multi-GPU packing uses the freest card. |

## Market maker details

Every quote, for the kind of job asked about (GPU or CPU):

- **Price**: congestion = 0.5 + (committed minutes in the next hour ÷ slot-minutes), clamped to
  0.5–4. An idle machine is half price to encourage filling it.
- **Menu**: for bids 1, 3, 5, 7 and 10, the queue position from the real ordering policy, the
  start time from a simulation of running leases and the queue, and the cost = predicted minutes
  × dominant share × bid/5 × congestion × surcharge.
- **Busy odds** at 15/30/60/120 min: 0.8 × (simulated schedule says full) + 0.2 × (how busy this
  hour of the week usually is, learned).
- **Cheapest window**: the next hour that is usually under 50% busy.
- **Advice**: the cheapest bid that starts within 10% (or 5 minutes) of the fastest; or `--spot`
  if the budget can't cover it.

Bids buy queue lead only under `las` and `stride` (at most `max_lead_minutes`). Under `lottery`
they buy tickets. Under `drf`, `fifo` and `srpt` they change only the price, and quotes show that.
