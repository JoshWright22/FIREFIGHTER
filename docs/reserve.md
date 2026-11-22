# The reserve, packing and priority: who sets them (decision, 2026-11-22)

## The question

On 2026-11-22 the GPU sat idle for 15+ minutes with three GPU jobs waiting, while 8.5 GB of RAM was
physically free and the card was half busy. Agents asked to change the human's reserve and packing
settings themselves, and the human (Josh) asked whether they should be allowed to: "maybe its better
that way idk, have an agent decide on the system design."

The question was: **should agents be able to change the human reserve and packing settings, or should
firefighter set them itself from measurements, so that no one has to?**

## Decision

**The human keeps the configuration, agents can't loosen it, and firefighter sizes the reserve and
packing it actually uses from measurements, within the bounds the human set. Suspending jobs under
memory pressure is the safety net.**

The human's target, in his own words: "firefighter should only be reserving enough for basic use of
my machine, browsing the web and such." So, outside a game:

| Resource | Held for the human (default) | Sized from |
|---|---|---|
| RAM | between `reserve.auto_min_gb` (1.5 GB) and `reserve.basic_gb` (3 GB), never above the configured value for the state | the human's measured 5-minute working-set rise in this state (physical RAM, never commit or private bytes) |
| VRAM | at most `reserve.basic_vram_gb` (1 GB) | the human's measured VRAM rise |
| CPU | the human's typical use (mean + 1), at most `reserve.basic_cpus` (4) and the configured value | measured on the CPUs kept for the human, which jobs are pinned off |

The reserve grows only when the human really starts something heavy. A game switches to the
configured game reserve (`min_free_gb` and `ram_margin_pct` apply again). CPU load past
`reserve.basic_cpus` on the human's own CPUs makes the CPU reserve follow that load. If RAM use rises
into the reserve, the RAM ladder reclaims the WSL/Docker VM's cache, then suspends jobs (on Windows
their memory goes to the page file, so no work is lost), and stops one only if RAM is critical and
nothing is left to suspend. `reserve.auto = false` brings back the fixed reserve.

## Why

**The human comes first, and agents have the wrong incentive.** An agent whose job waits on RAM has
every reason to lower the reserve and none to weigh the human's next five minutes. It also can't see
them: it sees one queue line, not the machine's history. Firefighter can see them, because it measures
the human's working set every 5 seconds, per state (away, present, active, game). Letting agents
loosen the reserve would make protecting the human depend on agents holding back. Measurement
doesn't have that weakness.

**Utilization needs a reserve that moves.** No single fixed value is right all day. 4 GB plus a 1.6 GB
margin was too much while the human browsed (it left 0.0 GB for jobs with 8.5 GB free) and is right in
a game. Only a reserve that follows measured use can be small most of the time and large when needed.
Agents asking for changes, and the human typing stopgaps at 3:45 AM, are a manual and late version of
that controller.

**Suspend-on-pressure makes a small reserve safe.** A reserve exists so the human isn't slowed down
before firefighter reacts. Firefighter now reacts within seconds: a suspended job is paged out and
resumes when RAM returns. So the reserve only has to cover the first few minutes of light use, which
is what it is now sized to cover.

**Simplicity.** There's one owner per setting. The human sets bounds once, with `firefighter config`
from their own terminal. Firefighter tunes inside them. Agents tune nothing and need no permission
model for it. `firefighter status` shows the reserve in force and which constraint binds (physical
RAM, commit, the human's reserve, VRAM, GPU packing), with its numbers, so nobody has to guess.

## Rules that came with it

- **Commit is not RAM.** Windows commit charge alone no longer holds jobs back while the page file can
  still grow: a system-managed page file may grow to 3× RAM, at most an eighth of its drive and the
  drive's free space less 10 GB. Commit still binds when the page file has a fixed size
  (`reserve.commit_free_gb` is kept beyond the largest it can grow to).
- **Packing uses measured load.** A second GPU job joins a card while the card's measured load
  (smoothed over 30 s) is under `gpu.pack_below_util` and that load plus the job's usual load stays
  under 150%. Past 100% the jobs only share the card's time.
- **Reservations decay.** A running job reserves RAM and VRAM it hasn't touched yet only at first.
  After 5 minutes the reservation closes in on its measured peak + 15% (halving every 10 minutes).
  Past its lease, or for a background project's started job, only what it really holds counts.
- **Declarations are believed** when some measured past run of the command fit within them (a history
  can mix configs). A declaration below every run ever measured is still overruled. Each `--config`
  file has its own history.
- **Backfill** behind a job blocked on VRAM or RAM is open to any job that fits until the blocked job
  has waited `order.max_wait_minutes`. After that only short jobs pass it, so it can't starve.
- **Priority.** Within a budget band the bid orders the line, and fairness (`order.policy`) orders
  equal bids. Waiting past `order.max_wait_minutes` still goes first, so nothing starves. Background
  projects (bots) wait behind project work as if they bid 2 (urgent excepted): those named in
  `order.background`, and those learned to be unattended recurring CPU-only work (6+ submits over 3+
  hours in a day with no agent or human above them, no GPU job in a week). `order.foreground` exempts
  a project.

## What agents may still change

With `guard.agents_can_configure` on, agents may change `order.*` (except `order.max_wait_minutes`),
`pricing.*` and `gpu.*`. Never `reserve.*`, `guard.*`, `budget.*` or `harvester.*`. With it off (the
default), agents change nothing, and the refusal says so.
