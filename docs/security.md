# Security notes

What the FIREFIGHTER guard trusts and what it doesn't. Short version: it's a referee for
cooperative agents, not a sandbox.


- The guard, `setup` and both installers refuse to run as administrator or root: any agent of
  yours can read the token and write its files, so anything elevated would lend those rights to
  every agent. It runs as you, binds only 127.0.0.1, and
  keeps its folder private (0700/0600 on Unix).
- It only acts on what a job's own client started (the caller is identified by its exact TCP
  connection, not by what it claims; only that client can start the job), on containers labeled
  for that job, and on processes adopted by you or, for its own session's work only, by an agent.
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
- Any agent can stop the guard (it's your process; it could kill it anyway). It comes back on the
  next `firefighter` call, and the new guard thaws anything the old one had suspended.
- Not a sandbox: an agent with your shell can do anything you can. FIREFIGHTER makes the polite
  path the cheap one and catches the careless one; it doesn't contain a hostile one.
- Windows: a job's whole process tree is in a job object, so nothing it starts outlives it by
  accident. Shared build servers on the `guard.keep_alive` list (MSBuild nodes, VBCSCompiler, ...)
  are spared when the job ends normally; if its client is killed, they go with it.
