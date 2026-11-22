package main

// Run flags, job submission, and command preparation.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/ui"
)

// ---- run --------------------------------------------------------------------------------------------

type runFlags struct {
	gpu, spot, detach, quiet, urgent, net, quick bool
	minutes, vram, ram                           float64
	bid                                          int
	name, project, evict, vendor                 string
	id, log, requestID                           string
}

func parseRun(args []string) (*runFlags, []string, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	r := &runFlags{}
	fs.BoolVar(&r.gpu, "gpu", false, "needs the GPU")
	fs.BoolVar(&r.spot, "spot", false, "leftovers only")
	fs.BoolVar(&r.detach, "detach", false, "return a job id at once")
	fs.BoolVar(&r.quiet, "quiet", false, "no status lines")
	fs.Float64Var(&r.minutes, "minutes", 0, "runtime estimate (lease)")
	fs.Float64Var(&r.vram, "vram", 0, "peak VRAM GB")
	fs.Float64Var(&r.ram, "ram", 0, "peak RAM GB")
	fs.IntVar(&r.bid, "bid", 5, "1-10; prefer --urgent (10) or the default (5)")
	fs.BoolVar(&r.urgent, "urgent", false, "the human is waiting on the result: starts sooner, costs 2x")
	fs.BoolVar(&r.quick, "quick", false, "a short look (under ~3 min), e.g. `docker run --rm img ls`: a short job, so it can start ahead of longer ones when RAM allows; the WSL/Docker VM's file cache it read is given back after")
	fs.BoolVar(&r.net, "net", false, "network-heavy (downloads, uploads); known installers and downloads are detected")
	fs.StringVar(&r.vendor, "gpu-vendor", "", "GPUs the job can use: nvidia, amd, intel or any (default: NVIDIA if present)")
	fs.StringVar(&r.name, "name", "", "job name")
	fs.StringVar(&r.project, "project", "", "project (default: the git repo or folder name)")
	fs.StringVar(&r.evict, "evict", "graceful", "never | graceful | force")
	fs.StringVar(&r.id, "id", "", "job id (internal)")
	fs.StringVar(&r.requestID, "request-id", "", "idempotency key: the same key (in the same project) never queues a second copy; a retry waits on the first")
	fs.StringVar(&r.log, "log", "", "log file (internal)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if fs.NArg() == 0 {
		return nil, nil, errors.New("nothing to run: firefighter run [flags] -- <command...>")
	}
	if r.urgent {
		r.bid = 10
	}
	if r.quick && r.minutes == 0 {
		r.minutes = 2
	}
	if r.quick && r.minutes > 3 {
		return nil, nil, errors.New("--quick is for looks under 3 minutes; give real work a --minutes lease instead")
	}
	switch strings.ToLower(r.vendor) {
	case "", "nvidia", "amd", "intel", "any":
	default:
		return r, nil, fmt.Errorf("--gpu-vendor must be nvidia, amd, intel or any (got %q)", r.vendor)
	}
	if r.evict != "never" && r.evict != "graceful" && r.evict != "force" {
		return nil, nil, fmt.Errorf("--evict must be never, graceful or force")
	}
	return r, fs.Args(), nil
}

func projectOf(dir string) string { return policy.ProjectOf(dir) }

// hashCmd names a command for learning its runtime and peaks: the program and its script, not
// its arguments. Shell wrappers (sh -c, cmd /c) are looked through, and docker runs are named by
// image, so unrelated commands in one project don't share a history.
func hashCmd(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if len(argv) >= 3 {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
		case "sh", "bash", "zsh", "cmd", "powershell", "pwsh":
			if f := strings.Fields(argv[len(argv)-1]); len(f) > 0 { // the inner command
				argv = f
			}
		}
	}
	// The program and the script or module it runs, by base name, so "python train.py" and
	// "python ./train.py" (or a full path) are one command to learn from, and "python -m pytest" and
	// "python -m torch.distributed.run train.py" are not.
	base := func(a string) string {
		return strings.TrimSuffix(strings.ToLower(filepath.Base(filepath.Clean(a))), ".exe")
	}
	parts := []string{base(argv[0])}
	for i := 1; i < len(argv) && len(parts) < 4; i++ {
		a := argv[i]
		if a == "-m" && i+1 < len(argv) {
			parts = append(parts, "-m", argv[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		parts = append(parts, base(a))
		if len(parts) == 2 || (len(parts) > 2 && parts[1] == "-m") {
			break
		}
	}
	// The config it runs, when it names one: one training script runs models of every size, and a
	// history mixing them made a 13M run wait for the RAM a large one had needed (Josh 11/22: 17.3 GB
	// counted for jobs peaking at 6-7).
	if cfg := configArg(argv[1:]); cfg != "" {
		parts = append(parts, "config", base(cfg))
	}
	key := strings.Join(parts, " ")
	if base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe"); (base == "docker" || base == "podman") && len(argv) > 1 && argv[1] == "run" {
		key = base + " run " + dockerImage(argv[2:])
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:6])
}

// configArg is the config file a command names: --config/--cfg/-c FILE (or =FILE), or else the
// first argument that is a .yaml, .yml, .toml or .json file.
func configArg(args []string) string {
	for i, a := range args {
		if a == "--" {
			break
		}
		for _, f := range []string{"--config", "--cfg", "-c", "--config-file", "--config-name"} {
			if a == f && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			if strings.HasPrefix(a, f+"=") {
				return strings.TrimPrefix(a, f+"=")
			}
		}
	}
	for _, a := range args {
		switch strings.ToLower(filepath.Ext(a)) {
		case ".yaml", ".yml", ".toml", ".json":
			if !strings.HasPrefix(a, "-") {
				return a
			}
		}
	}
	return ""
}

// dockerImage finds the image in `docker run` arguments: the first argument that isn't a flag
// or a flag's value.
func dockerImage(args []string) string {
	if i := daemon.DockerImageAt(args); i >= 0 {
		return args[i]
	}
	return ""
}

// detachArgs rebuilds the child's command line from the parsed flags (never by filtering raw
// args, which could also hit the job's own flags after --).
func detachArgs(r *runFlags, argv []string) []string {
	out := []string{"--minutes", fmt.Sprint(r.minutes), "--bid", fmt.Sprint(r.bid), "--evict", r.evict}
	if r.gpu {
		out = append(out, "--gpu")
	}
	if r.spot {
		out = append(out, "--spot")
	}
	if r.vendor != "" {
		out = append(out, "--gpu-vendor", r.vendor)
	}
	if r.net {
		out = append(out, "--net")
	}
	if r.quick {
		out = append(out, "--quick")
	}
	if r.vram > 0 {
		out = append(out, "--vram", fmt.Sprint(r.vram))
	}
	if r.ram > 0 {
		out = append(out, "--ram", fmt.Sprint(r.ram))
	}
	if r.name != "" {
		out = append(out, "--name", r.name)
	}
	if r.project != "" {
		out = append(out, "--project", r.project)
	}
	return append(append(out, "--"), argv...)
}

func run(args []string) (int, error) {
	r, argv, err := parseRun(args)
	if err != nil {
		return 2, err
	}
	if r.minutes <= 0 {
		ui.Say(ui.Ivory, "no --minutes given: assuming 30. Pass an honest estimate; it's your lease.")
		r.minutes = 30
	}
	if r.requestID != "" && r.id == "" {
		cwd, _ := os.Getwd()
		project := r.project
		if project == "" {
			project = projectOf(cwd)
		}
		r.id = daemon.RequestID(project, r.requestID)
		if c, err := client.Connect(); err == nil {
			if v, err := c.Get(r.id, "", 0); err == nil { // submitted before: never a second copy
				if r.detach {
					fmt.Println(r.id)
					ui.Note("request %q was already submitted as %s (%s); `firefighter wait %s` for the receipt", r.requestID, r.id, v.State, r.id)
					return 0, nil
				}
				ui.Note("request %q was already submitted as %s (%s): waiting on it", r.requestID, r.id, v.State)
				return wait([]string{r.id})
			}
		}
	}
	if r.detach {
		cwd, _ := os.Getwd()
		if r.id == "" {
			r.id = daemon.NewID()
		}
		id, log, err := client.DetachedID(r.id, detachArgs(r, argv), cwd)
		if err != nil {
			return 1, err
		}
		if r.requestID != "" { // registered before returning, so a retry right after finds it
			if c, err := client.Connect(); err == nil {
				_, _ = c.GetSoon(id)
			}
		}
		fmt.Println(id)
		ui.Note("queued as %s; output in %s; `firefighter wait %s` for the receipt", id, log, id)
		return 0, nil
	}
	watchLauncher(r.log != "") // the internal --log form is the --detach child
	c, err := client.Connect()
	if err != nil {
		return 1, err
	}
	cwd, _ := os.Getwd()
	if r.project == "" {
		r.project = projectOf(cwd)
	}
	if r.name == "" {
		r.name = r.project + " " + filepath.Base(argv[0])
	}
	if r.id == "" {
		r.id = daemon.NewID()
	}
	if daemon.DockerDetachedArgs(argv) || daemon.DockerDetached(shellInner(argv)) {
		// The CLI would return at once and the container would run on unseen: never governed,
		// never stopped for the human (review 11/20).
		return 2, errors.New("docker run -d returns at once while the container keeps running outside firefighter's sight: drop -d (to not wait yourself, use firefighter run --detach)")
	}
	argv, container := dockerize(argv, r.id)
	t := policy.Ticket{ID: r.id, Project: r.project, Name: r.name, Cwd: cwd, Command: strings.Join(argv, " "), CmdHash: hashCmd(argv),
		GPU: r.gpu, VRAMGB: r.vram, RAMGB: r.ram, Minutes: r.minutes, Bid: r.bid, Evict: r.evict, Spot: r.spot, Quick: r.quick, ClientPID: os.Getpid(), WantVendor: r.vendor, Net: r.net || netCommand(argv), NetAuto: !r.net && netCommand(argv)}
	for attempt := 1; ; attempt++ {
		code, requeue, err := turn(c, &t, argv, container, r.quiet) // &t: an amend while waiting carries into the next attempt
		if err != nil || !requeue {
			return code, err
		}
		ui.Say(ui.Coral, "stopped to give the human the machine back; back in line automatically (attempt %d). Nothing to do: it restarts when there's room", attempt+1)
	}
}

// shellInner is the command string a shell wrapper runs (sh -c "...", cmd /c "...", powershell
// -Command "..."), or "" when argv isn't one: every MCP submit is wrapped like that.
func shellInner(argv []string) string {
	if len(argv) < 3 {
		return ""
	}
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
	case "sh", "bash", "zsh", "dash", "cmd", "powershell", "pwsh":
		for i, a := range argv[1 : len(argv)-1] {
			switch strings.ToLower(a) {
			case "-c", "/c", "/k", "-command", "-lc":
				return strings.Join(argv[i+2:], " ")
			}
		}
	}
	return ""
}

// dockerize labels `docker run` / `podman run` so the guard can find, pause, pin and kill the container.
func dockerize(argv []string, id string) ([]string, string) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	if (base == "docker" || base == "podman") && len(argv) >= 3 && argv[1] == "container" && argv[2] == "run" {
		argv = append([]string{argv[0], "run"}, argv[3:]...) // `docker container run` is `docker run`: labeled too
	}
	if (base != "docker" && base != "podman") || len(argv) < 2 || argv[1] != "run" {
		return argv, ""
	}
	name := ""
	end := len(argv)
	if k := daemon.DockerImageAt(argv[2:]); k >= 0 {
		end = k + 2
	}
	for i, a := range argv[:end] {
		if a == "--name" && i+1 < len(argv) {
			name = argv[i+1]
		} else if strings.HasPrefix(a, "--name=") {
			name = strings.TrimPrefix(a, "--name=")
		}
	}
	extra := []string{"--label", "firefighter.job=" + id}
	if name == "" {
		name = "firefighter-" + id
		extra = append(extra, "--name", name)
	}
	out := append([]string{argv[0], "run"}, extra...)
	return append(out, argv[2:]...), name
}

// netCommand recognizes commands that are mostly network traffic: image pulls, package installs,
// clones, downloads. (`firefighter config set network.auto_detect false` turns this off; the
// daemon ignores the flag then.)
func netCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	text := strings.Join(argv, " ")
	if len(argv) >= 3 {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
		case "sh", "bash", "zsh", "cmd", "powershell", "pwsh":
			text = argv[len(argv)-1]
		}
	}
	// Each step of a compound command ("cd x && pip install -r r.txt").
	for _, seg := range strings.FieldsFunc(text, func(r rune) bool { return r == '&' || r == ';' || r == '|' }) {
		if netStep(strings.Fields(seg)) {
			return true
		}
	}
	return false
}

func netStep(words []string) bool {
	if len(words) == 0 {
		return false
	}
	prog := strings.TrimSuffix(strings.ToLower(filepath.Base(words[0])), ".exe")
	has := func(subs ...string) bool { // a subcommand anywhere after options (pip --index-url U install)
		for _, w := range words[1:] {
			for _, s := range subs {
				if strings.EqualFold(w, s) {
					return true
				}
			}
		}
		return false
	}
	switch prog {
	case "curl", "wget", "aria2c", "rsync", "scp", "huggingface-cli", "hf", "gsutil", "rclone", "azcopy":
		return true
	case "aws":
		return has("s3") && has("cp", "sync", "mv")
	case "docker", "podman":
		return has("pull", "push") // build and compose up are mostly CPU
	case "git":
		return has("clone", "fetch", "pull", "lfs")
	case "pip", "pip3", "pipx", "npm", "pnpm", "yarn", "bun", "poetry", "conda", "mamba", "micromamba", "brew", "winget",
		"choco", "scoop", "apt", "apt-get", "dnf", "ollama":
		return has("install", "i", "ci", "add", "pull", "update", "upgrade", "sync", "create")
	case "cargo":
		return has("fetch") // install compiles: CPU-bound
	case "uv":
		return has("pip", "sync", "add")
	case "go":
		return has("get", "download")
	case "python", "python3", "py":
		return has("pip") && has("install") || strings.Contains(strings.Join(words, " "), "huggingface_hub")
	}
	return false
}
