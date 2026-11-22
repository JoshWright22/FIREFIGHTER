<p align="center"><img src="assets/logo.svg" alt="FIREFIGHTER: lets several AI coding agents share one computer" width="640"></p>

<p align="center">
  <a href="https://github.com/JoshWright22/FIREFIGHTER/actions/workflows/ci.yml"><img src="https://github.com/JoshWright22/FIREFIGHTER/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/JoshWright22/FIREFIGHTER/releases"><img src="https://img.shields.io/github/v/release/JoshWright22/FIREFIGHTER?color=d4147a&label=release" alt="release"></a>
  <img src="https://img.shields.io/badge/Windows%20%C2%B7%20macOS%20%C2%B7%20Linux-141216" alt="Windows, macOS, Linux">
  <img src="https://img.shields.io/badge/MCP-server-d4147a" alt="MCP server">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-141216" alt="MIT"></a>
</p>

<h3 align="center">Run several AI coding agents on one computer. They take turns, and you always come first.</h3>

<p align="center"><b>FIREFIGHTER</b> is a small program that runs in the background. AI coding agents
(<b>Claude Code, Codex, Gemini CLI, Cursor, Grok, OpenCode, Kimi, Antigravity, Copilot, T3 Code</b> and more)
hand it their heavy jobs. It runs each job when there is room, keeps part of your computer free for you,
and pauses the agents' work when you need it.<br>
One command to install, on <b>Windows, macOS and Linux</b>.</p>

<p align="center"><img src="assets/demo.svg" alt="Animated demo: agents wait their turn for the graphics card, the human starts a game, the agents' training pauses, everyone finishes" width="860"></p>

## The problem

AI coding agents don't just write code. They also run things: train a model, run a big test suite,
build a Docker image, download a dataset. Run two or three agents at once and each one decides on
its own when to start that heavy work. They all grab the same graphics card (GPU), its memory
(VRAM), your processor (CPU) and your memory (RAM) at the same moment. Jobs run out of memory and
crash, they slow each other to a crawl, your game stutters, a video call drops, and nobody can tell
which agent is using what.

## What FIREFIGHTER does

- **One line for everyone.** Agents send heavy jobs to FIREFIGHTER instead of starting them
  directly. Each job waits its turn, and every agent can see what the others are running.
- **A share kept for you.** Part of the processor, memory, graphics card and network is always kept
  free for you. When you start typing, open a game or join a call, that share grows quickly and the
  agents' jobs pause, then continue when you're done.
- **Nothing lost when it has to make room.** A job is paused, frozen, or asked to save its progress
  and go back in line. Stopping a job outright is the last resort.
- **Plain answers for the agents.** Replies tell the agent when its job will start, why it is
  waiting, and what to do next, so it doesn't guess or start the job a second time.

## Install (one command)

**Windows** (PowerShell):

```powershell
irm https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/v0.3.3/scripts/install.ps1 | iex
```

**macOS / Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/v0.3.3/scripts/install.sh | sh
```

**With Go 1.25 or newer** (Go's bin folder must be on your PATH):

```sh
go install github.com/JoshWright22/firefighter/cmd/firefighter@latest && firefighter setup
```

The installers check that the download is genuine (its checksum, and its signature when the GitHub
CLI is signed in), install the program and run `firefighter setup`. You don't need admin rights.
Setup starts FIREFIGHTER when you log in and teaches every AI agent it finds on your computer to use
it. On macOS and Linux, open a new terminal afterwards so it's on your PATH. A copy built with
`go install` shows version `dev` and doesn't update itself.

Then run `firefighter doctor`: it checks each part and tells you how to fix anything that's off.
`firefighter status` shows what's happening right now.

To remove it, run `firefighter uninstall`. That stops it and removes what setup added (the start at
login, the agents' settings and instructions). It leaves the program, any PATH entry and its data
folder; delete those yourself if you want them gone. Where they are:
[install details](docs/reference.md#install-details).

## How it works

You mostly don't do anything. Setup teaches your agents to send heavy work through FIREFIGHTER,
either with the `firefighter run` command or through its MCP tools (MCP is the standard way AI agents
connect to outside tools).

1. **An agent asks for a turn.** It says roughly how long the job takes (`--minutes`) and, for
   graphics work, how much graphics memory it needs (`--vram`). The project that has used the
   least lately goes first. Urgent and "whenever" jobs get their own lanes.
2. **The job starts when it fits.** FIREFIGHTER checks the job's needs (what the agent said, and
   what the same command used before) against what's free after your share. On a computer with
   several graphics cards it picks one.
3. **You come back, the job gets out of the way.** Keyboard and mouse use and games grow your share.
   Running jobs are paused, frozen in place, or asked to save and leave; they go back in line on
   their own. (Detecting fullscreen games is Windows-only; on macOS and Linux, list your games in
   `reserve.exclusive_apps`.)
4. **The job ends with a receipt.** How long it ran, its peak memory, and, if it failed, why and what
   to change.

**What an agent sees** when it runs a job (FIREFIGHTER's lines start with ◆; the rest is the job's
own output):

```text
$ firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
◆ 3f2a91c0 nanoGPT train waiting #2, starts ~2:05 PM (in 22m): GPU turn queue
   gpu 40m: start ~22m, cost ~42 of 212 (urgent ~6m ~84) (spot ~22m ~14) | busy 15/60/120m 79/35/12% | quiet 02:00 (usually 8% busy) | do: queue it at normal (starts ~22m); keep working meanwhile
◆ nanoGPT train: spotlight's yours (lease 40 min, budget left 212.0)
step 1200 | loss 2.913 ...
◆ nanoGPT train: FIREFIGHTER needs it to stop (VRAM below the human's 8.0 GB reserve); it gets time to save and exit, then it's requeued
◆ nanoGPT train: curtain call, evicted (exit 75) in 12.0 min (declared 40), waited 22.0, charged 31.4 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
◆ stopped to give the human the machine back; back in line automatically (attempt 2). Nothing to do: it restarts when there's room
...
◆ nanoGPT train: spotlight's yours (lease 40 min, budget left 180.6)
◆ nanoGPT train: curtain call, ok (exit 0) in 21.4 min (declared 40), waited 6.1, charged 18.2 machine-min, peak 3.1 GB RAM / 9.4 GB VRAM
```

The job waited about 22 minutes for its turn, trained, was asked to save and step aside when the
human needed the graphics memory, went back in line by itself, and finished.

**What you see** with `firefighter status` while you play a game:

```text
$ firefighter status
FIREFIGHTER  human: on stage (game/media) eldenring.exe using 64% of the 3D engine
  reserve for the human: 6 CPUs, 35% RAM, 8.0 GB VRAM
  CPU 9.2/16 busy (5.1 human+other)  RAM 24.0/63.9 GB free  GPU 71%  VRAM 14.9/24.0 GB
  policy: preset balanced, order las, pricing priced, budget karma
  gpu 30m: start ~1.5h, cost ~120 of 168 (spot ~2.1h ~40) | busy 15/60/120m 96/92/64% | quiet 02:00 (usually 8% busy) | do: queue it (starts ~1.5h), or spot if it can wait, or plan it for 02:00
  cpu 30m: start ~1m, cost ~4 of 168 (spot ~1m ~1) | busy 15/60/120m 12/20/25% | do: queue it at normal (starts ~1m); keep working meanwhile

on stage
  3f2a91c0 nanoGPT train                      nanoGPT        until ~2:45 PM paused: the human needs the GPU (eldenring.exe using 64% of the 3D engine)

in the wings
  a81c0d42 #1 eval sweep                      evals          on unknown the human needs the GPU (eldenring.exe using 64% of the 3D engine)
  77d0e5b1 #1 docs build                      site           on unknown needs 6.0 GB RAM, 1.6 GB free after the human's reserve (it starts with 3.2 GB to spare)

budgets (machine-minutes left)
  default                  168.0
  nanoGPT                  180.6
  evals                    168.5
  site                     240.0
```

"On stage" is what's running (here, paused for your game); "in the wings" is what's waiting and why.
A start time reads "unknown" when it depends on something only you control, like when you stop
playing. The full details are in [How it works](docs/how-it-works.md).

## What you get

**For you**

- **Your computer stays yours.** A share of the processor, memory and graphics card is kept free
  for you. It follows what you're doing (typing, games, calls) and also makes room for your other
  programs, Docker included.
- **No freezes from memory running out.** Short of memory, FIREFIGHTER first takes back memory
  nobody is using (on Windows, the cache of the WSL/Docker virtual machine), then freezes a job and
  frees its memory so it can continue later. It also keeps free space on your drives.
- **Disk space you hand out.** Give agents a folder and a size (`storage.gb`). Each project claims a
  part of it, labels what it saves (keep, useful or scratch) and what each thing is for, and deletes
  the least important first. Anything labeled keep is never deleted by `storage free`. A project over
  its limit can't start new jobs until it frees space.
- **One check-up command.** `firefighter doctor` checks the program, the background service, your
  settings, each agent's setup, graphics cards, disks, Docker and storage, and prints the fix for
  anything that's wrong.
- **Optional: earn from an idle graphics card.** Salad, BOINC or similar can run while nothing else
  needs the graphics card. Give it your electricity price and it runs only while it earns more than
  the power costs (`firefighter harvest`).

**For your agents**

- **Turns instead of fights.** One line, shared fairly between projects. By default a job that has
  waited 45 minutes moves ahead of newer ones (it still waits for room, and for your game to end).
- **Simple prices.** Each project has a daily budget. By default, "spot" jobs that can wait cost a
  third and "urgent" ones, for when you're waiting on the result, cost double.
- **Honest waiting.** A waiting job is told exactly what it waits for, with numbers, and its start
  time is "unknown" when nothing can predict it. `firefighter amend` fixes a waiting job's estimate
  without losing its place.
- **Failures explained.** When a job fails on its own, the receipt says why (out of graphics memory,
  out of memory or disk, a missing program, a network error, Docker not running, a crash), whether
  running it again as-is can work, and what to change. Out of memory, the next run automatically
  asks for more.
- **Retries never double up.** `--request-id` gives a job a name; an agent that retries after a
  timeout gets the job it already queued (kept for 48 hours) instead of starting a second copy.
- **It learns.** Each run teaches it what that command needs, and your own use teaches it how much
  to keep free for you. `firefighter forecast` shows what it expects.
- **Docker containers count.** A container a job starts counts toward that job's memory, and
  `run --quick` is the cheap way to take a short look inside an image.
- **Work started outside the line is caught.** Heavy programs an agent starts without FIREFIGHTER
  are brought into the line, charged extra and paused first.
- **Downloads take turns too,** and give way to your games and calls.

**For everyone**

- **It keeps itself up to date.** New releases are checked (checksum, and signature when the GitHub
  CLI is signed in) and installed automatically; it restarts as the new version only when no job is
  running. `update.auto = false` turns this off.
- **Works on any computer.** Windows, macOS and Linux; NVIDIA, AMD or Intel graphics, any number of
  cards. Everything runs on your machine; only update checks use the internet.
- **Set and forget.** It starts when you log in. On Linux it uses a user systemd service when there
  is one, otherwise the desktop's autostart.

## Agents it sets up

Claude Code, Codex, Gemini CLI, Antigravity, Cursor, Grok Build, OpenCode, Kimi CLI and Kimi Code,
Pi, Muse Code, fx, Kiro, Windsurf/Devin, Copilot CLI, Amp, Qwen Code, Crush, Cline, Goose and Droid,
and apps built on them like T3 Code. Each gets what it supports: an MCP connection, a short block of
instructions in its settings, or both.

## Commands

```sh
firefighter run --gpu --minutes 40 --vram 10 --name "nanoGPT train" -- python train.py
firefighter run --gpu --urgent --minutes 10 -- python eval.py     # you're waiting on it: costs double
firefighter run --gpu --spot --minutes 120 -- python sweep.py     # can wait: costs a third
firefighter run --detach --gpu --minutes 90 -- python sweep.py    # returns at once with an id; then: firefighter wait <id>
firefighter run --request-id eval-42 --minutes 10 -- python e.py  # a retry with the same id never starts a second copy
firefighter quote --gpu --minutes 30 --vram 8                     # when it would start and what it costs; queues nothing
firefighter status                                                # what's running, what's waiting, budgets
firefighter doctor                                                # check everything; prints the fix for each problem
firefighter extend <id> 15                                        # a job is taking longer: say so
firefighter amend <id> --vram 8                                   # fix a waiting job's estimate without losing its place
firefighter run --quick -- docker run --rm img ls /data           # a short look inside a Docker image
firefighter plan "eval sweep" --when 22:00 --minutes 120 --gpu 90 --vram 12   # announce big work for later
firefighter storage claim --gb 20 --why "datasets"                # take disk space from what the human gave agents
firefighter storage label data/raw --rank useful --for "training set, 2 h to download"
firefighter storage free --gb 5                                   # delete scratch first; keep is never deleted
firefighter harvest                                               # the idle-graphics-card harvester's money
firefighter config set preset gamer                               # balanced, gamer, throughput or fair
firefighter protocol                                              # how GPU jobs can pause and save cleanly
```

Agents connected through MCP get six tools: `firefighter_submit`, `firefighter_wait`,
`firefighter_status`, `firefighter_job`, `firefighter_plan` and `firefighter_storage`
(`firefighter_config` too, only if you allow agents to change settings with
`guard.agents_can_configure`; `firefighter config set` from an agent's session follows the same rule). Every command and option: [CLI reference](docs/reference.md#cli-reference).

## Docs

- [How it works](docs/how-it-works.md): every feature in detail, the rules for who goes first, how jobs are paused and stopped, and the research behind the numbers
- [Reference](docs/reference.md): install details, every command and option, files, what works on each OS, comparison with other tools, FAQ
- [Security](docs/security.md): what FIREFIGHTER trusts and what it doesn't
- [Integrations](docs/integrations.md): status line, event stream, HTTP API, Claude Code plugin, VS Code extension
- [Research report](docs/research.md): the original survey of related projects and papers, with sources

<p align="center"><img src="assets/icon.svg" width="72" alt=""><br><sub>MIT · built for people who run too many agents at once</sub></p>
