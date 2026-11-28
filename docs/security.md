# Security notes

What the FIREFIGHTER guard trusts and what it doesn't. Short version: it's a referee for
cooperative agents, not a sandbox.


- The guard, `setup` and both installers refuse to run as administrator or root: any agent of
  yours can read the token and write its files, so anything elevated would lend those rights to
  every agent. It runs as you, binds only 127.0.0.1, and
  keeps its folder private (0700/0600 on Unix).
- It only acts on what a job's own client started (the caller is identified by its exact TCP
  connection, not by what it claims; only that client can start the job), on containers proven to
  be that job's by the `firefighter.job=<id>` label (`firefighter run -- docker run` adds it; a job's
  script can set it from `FIREFIGHTER_JOB_ID`; a container matched only by name or timing is counted,
  never paused, capped or stopped), and on
  processes adopted by you or, for agent work only, by an agent (`--force` never reaches your other
  programs).
  Ancestry and ownership checks ignore reused pids. "You" means a caller whose unbroken process
  chain reaches a terminal window (Windows Terminal, iTerm, GNOME Terminal, ...; or sshd) with no
  agent CLI, script runtime (node) or IDE in between (`guard.human_hosts`, `guard.agent_hosts`).
  The terminal must be installed by an administrator: the program and every folder above it owned
  by the system (root on macOS/Linux; TrustedInstaller, SYSTEM or Administrators on Windows), or a
  real sshd session started by root (a Linux text-console login, whose parent is init, doesn't
  count: an agent's detached process ends up there too). A terminal installed just for your user (a kitty or
  WezTerm download, Scoop, an app you dragged into /Applications, a Nix profile) doesn't count. The desktop shell, a classic console
  started from it, tmux and screen don't count either (an agent can open those silently); from
  them, add `--force` for other projects' jobs and for `adopt`. Anything else, including detached helpers
  and agents hosted in node or an IDE, is an agent session limited to its own work and its project;
  from an IDE terminal, add `--force` to manage another project's job. An agent that opens a window
  in a terminal you already have running (`wt.exe`, `open -a Terminal`, a silent `ssh localhost`)
  passes for you there too. These are guardrails against accidents, not a boundary against a hostile
  agent with your shell.
- `firefighter_submit` runs a command as you, outside an agent's own sandbox; its MCP annotations
  say so, so hosts can ask for approval as they do for a shell.
- Releases are built by GitHub Actions with SHA-pinned actions, the Go toolchain from go.mod, a
  locked npm toolchain for the extension, and least-privilege tokens, and carry signed build
  provenance. The installers check it automatically when the GitHub CLI is installed and signed in;
  by hand: `gh attestation verify firefighter_windows_amd64.zip --repo JoshWright22/FIREFIGHTER`.
  Every release the installers can fetch (v0.2.0 on) is signed, so with the CLI signed in a
  missing or failing attestation stops the install (a replaced archive and checksums.txt look
  exactly like an unsigned release). Without the GitHub CLI only the checksum is checked.
- **Release signing (since 2026-11-22).** A release installs itself only when its `checksums.txt`
  carries an ed25519 signature (`checksums.txt.sig`) from a key built into the program
  (`internal/update/sign.go`), and the archive matches those checksums. No trusted key, no signature
  or a bad one, and nothing is installed, with or without the GitHub CLI. This is what stops someone,
  or an agent, who can push a `v*` tag: GitHub's build provenance only proves "built by our
  workflow from some tag", and the release's own checksums only catch corruption. The key is made
  and used offline by the maintainer, and its private half is encrypted with a passphrase no agent
  knows:
  `go run ./scripts/releasekey gen <key file>` (prints the public key for `sign.go`) and, for each
  release, `go run ./scripts/releasekey sign <key file> checksums.txt`, then upload
  `checksums.txt.sig` to the release. Until a key is embedded, the guard installs no update at all.
  With the GitHub CLI signed in, build provenance is checked as well. The guard never installs an
  older version and only asks this repository's releases over HTTPS. The test-only override of the
  release address is ignored in your real state folder and only accepts a server on this machine.
  The installer and updater reject a release that fails its launch check and retain the current copy.
- The install one-liners are pinned to a tag (`.../FIREFIGHTER/v0.3.3/scripts/install.ps1`), and
  each script installs the release it names, never "latest". `release.yml` refuses to publish a
  tag the scripts don't name. Recommended on GitHub: a tag ruleset that lets only the maintainer
  create `v*` tags (Settings > Rules > Rulesets).
- Microsoft Defender has quarantined unsigned Windows builds with `Behavior:Win32/Persistence.A!ml`
  and `Trojan:Win32/Bearfoos` verdicts. The Windows installer registers logon startup through a
  per-user Startup shortcut and retires earlier logon entries. The CLI starts the guard on demand.
  Authenticode adds publisher identity and integrity. Report any false positives for the exact
  affected file to Microsoft. [Microsoft Security Intelligence](https://www.microsoft.com/wdsi/filesubmission)
- **Harvester commands need the human's approval.** The idle harvester's `start`, `stop` and
  `running` commands in `firefighter.toml` are shell lines the guard runs at logon and whenever the
  GPU goes idle. They run only if their SHA-256 is in `harvester-approved` in the state folder,
  which only `firefighter harvest approve` writes, from your own terminal, after showing the exact
  commands. A program that edits `firefighter.toml` can't plant one that runs at every logon.
  `firefighter status` says when configured commands aren't approved.
- Setup edits other tools' JSON configs one entry at a time: every other key keeps its place and
  value, an unchanged file isn't rewritten, a file another program saved meanwhile is read again,
  and each write leaves a timestamped backup beside the file (the newest 3, plus the original
  `.firefighter.bak`).
- The local API compares its token in constant time.
- To free RAM, the guard runs two fixed commands as root inside an already running WSL distro
  (`cat /proc/meminfo`, and writes to `/proc/sys/vm/drop_caches` and `compact_memory`) through the
  system's own `wsl.exe`. It never starts a distro and passes nothing an agent supplies.
  `reserve.reclaim_vm_cache false` turns it off.
- Any agent can stop the guard (it's your process; it could kill it anyway). It comes back on the
  next `firefighter` call, and the new guard thaws anything the old one had suspended.
- Not a sandbox: an agent with your shell can do anything you can. FIREFIGHTER makes the polite
  path the cheap one and catches the careless one; it doesn't contain a hostile one.
- Windows: a job's whole process tree is in a job object, so nothing it starts outlives it by
  accident. If Windows refuses the job object (rare: some sandboxes), the job still runs and the
  receipt says what it starts may outlive it. Shared build servers on the `guard.keep_alive` list (MSBuild nodes, VBCSCompiler, ...)
  are spared when the job ends normally; if its client is killed, they go with it.
