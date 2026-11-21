package daemon

import (
	"path/filepath"
	"strings"
)

// dockerFlags are `docker run` options that never take a value. Any other "--opt" without "="
// takes the next argument: docker has many more valued options than flags, so this errs toward
// finding the real image (an unknown valued option must not make its value look like the image).
var dockerFlags = map[string]bool{"--detach": true, "--interactive": true, "--tty": true, "--rm": true, "--privileged": true,
	"--init": true, "--read-only": true, "--publish-all": true, "--no-healthcheck": true, "--oom-kill-disable": true,
	"--help": true, "--quiet": true, "--disable-content-trust": true, "--replace": true, "--sig-proxy": true}

// dockerGlobalValued are docker/podman options before the verb that take a separate value.
var dockerGlobalValued = map[string]bool{"--context": true, "-c": true, "--host": true, "-H": true, "--config": true,
	"--log-level": true, "-l": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
	"--connection": true, "--url": true, "--identity": true, "--root": true, "--runroot": true, "--storage-driver": true}

// shortFlags are the one-letter options that take no value (-d -i -t -P -q), so a cluster like
// -dit is all flags, and -dp 8080:80 ends in a valued one.
const shortFlags = "ditPq"

// DockerImageAt is the index of the image in `docker run` arguments (those after "run"), -1 if
// none: everything before it is the container's options, everything after it the program inside.
func DockerImageAt(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		case strings.HasPrefix(a, "--"):
			if !strings.Contains(a, "=") && !dockerFlags[a] {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Short options: a cluster of flags (-it, -d, -P, -q), possibly ending in one valued
			// option (-dp 8080:80, -itv /a:/b), or a valued option with its value attached (-p80:80).
			rest := a[1:]
			k := strings.IndexFunc(rest, func(c rune) bool { return !strings.ContainsRune(shortFlags, c) })
			if k >= 0 && k == len(rest)-1 && !strings.Contains(a, "=") {
				i++ // the last letter takes the next argument
			}
		default:
			return i
		}
	}
	return -1
}

// dockerRunAt finds `docker|podman [global opts] [container] run|create` in fields and returns
// the index of the first argument after the verb (-1 if the command runs no container).
func dockerRunAt(f []string) int {
	for i, a := range f {
		base := strings.TrimSuffix(strings.ToLower(filepathBase(a)), ".exe")
		if base != "docker" && base != "podman" {
			continue
		}
		j := i + 1
		for j < len(f) && strings.HasPrefix(f[j], "-") { // global options before the verb
			if dockerGlobalValued[f[j]] && !strings.Contains(f[j], "=") {
				j++ // its value (docker --context x run ...)
			}
			j++
		}
		if j < len(f) && f[j] == "container" { // docker container run
			j++
		}
		if j < len(f) && (f[j] == "run" || f[j] == "create") {
			return j + 1
		}
	}
	return -1
}

// DockerRunOptions returns the container's own options of the first `docker run` / `podman run`
// (or create, or `docker container run`) in a command line: the arguments up to the image. A
// `--memory=64m` after the image belongs to the program inside. ok is false when the command runs
// no container.
func DockerRunOptions(command string) (opts []string, ok bool) {
	return DockerRunArgs(strings.Fields(command))
}

// DockerRunArgs is DockerRunOptions on an argv that is still split as the OS will pass it (an
// option value with spaces stays one argument; review 11/20).
func DockerRunArgs(f []string) (opts []string, ok bool) {
	at := dockerRunAt(f)
	if at < 0 {
		return nil, false
	}
	args := f[at:]
	if k := DockerImageAt(args); k >= 0 {
		args = args[:k]
	}
	return args, true
}

// DockerDetached says whether a command's docker run starts its container detached (-d): the CLI
// would return at once while the work runs on, out of firefighter's sight.
func DockerDetached(command string) bool {
	return DockerDetachedArgs(strings.Fields(command))
}

// DockerDetachedArgs is DockerDetached on an argv as the OS passes it.
func DockerDetachedArgs(argv []string) bool {
	opts, ok := DockerRunArgs(argv)
	if !ok {
		return false
	}
	for i := 0; i < len(opts); i++ {
		a := opts[i]
		switch {
		case a == "--detach" || a == "-d":
			return true
		case strings.HasPrefix(a, "--detach="):
			return strings.TrimPrefix(a, "--detach=") != "false" && strings.TrimPrefix(a, "--detach=") != "0"
		case strings.HasPrefix(a, "-d="):
			return strings.TrimPrefix(a, "-d=") != "false" && strings.TrimPrefix(a, "-d=") != "0"
		case strings.HasPrefix(a, "--"):
			if !strings.Contains(a, "=") && !dockerFlags[a] {
				i++ // its value
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			rest := a[1:]
			k := strings.IndexFunc(rest, func(c rune) bool { return !strings.ContainsRune(shortFlags, c) })
			flagsPart := rest
			if k >= 0 {
				flagsPart = rest[:k]
			}
			if strings.ContainsRune(flagsPart, 'd') {
				return true // -dit, -itd, -dp 8080:80
			}
			if k >= 0 && k == len(rest)-1 && !strings.Contains(a, "=") {
				i++ // the last letter takes the next argument
			}
		}
	}
	return false
}

// filepathBase is filepath.Base for both separators (a Windows path seen on Linux too).
func filepathBase(p string) string {
	return filepath.Base(strings.ReplaceAll(strings.Trim(p, `"`), `\`, "/"))
}
