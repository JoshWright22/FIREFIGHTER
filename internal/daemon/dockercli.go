package daemon

import (
	"path/filepath"
	"strings"
)

// dockerValueFlags are `docker run` / `podman run` options that take a separate value, so the
// token after them is that value, not the image.
var dockerValueFlags = map[string]bool{
	"-a": true, "--attach": true, "--add-host": true, "--blkio-weight": true, "--cap-add": true, "--cap-drop": true,
	"--cgroup-parent": true, "--cgroupns": true, "--cidfile": true, "--cpu-period": true, "--cpu-quota": true,
	"--cpu-shares": true, "-c": true, "--cpus": true, "--cpuset-cpus": true, "--cpuset-mems": true, "--device": true,
	"--device-cgroup-rule": true, "--dns": true, "--dns-option": true, "--dns-search": true, "--domainname": true,
	"--entrypoint": true, "-e": true, "--env": true, "--env-file": true, "--expose": true, "--gpus": true,
	"--group-add": true, "--health-cmd": true, "--health-interval": true, "--health-retries": true,
	"--health-start-period": true, "--health-timeout": true, "-h": true, "--hostname": true, "--ip": true, "--ip6": true,
	"--ipc": true, "--isolation": true, "--kernel-memory": true, "-l": true, "--label": true, "--label-file": true,
	"--link": true, "--log-driver": true, "--log-opt": true, "--mac-address": true, "-m": true, "--memory": true,
	"--memory-reservation": true, "--memory-swap": true, "--memory-swappiness": true, "--mount": true, "--name": true,
	"--network": true, "--net": true, "--network-alias": true, "--oom-score-adj": true, "--pid": true,
	"--pids-limit": true, "--platform": true, "-p": true, "--publish": true, "--pull": true, "--restart": true,
	"--runtime": true, "--security-opt": true, "--shm-size": true, "--stop-signal": true, "--stop-timeout": true,
	"--storage-opt": true, "--sysctl": true, "--tmpfs": true, "--ulimit": true, "-u": true, "--user": true,
	"--userns": true, "--uts": true, "-v": true, "--volume": true, "--volume-driver": true, "--volumes-from": true,
	"-w": true, "--workdir": true,
}

// DockerRunOptions returns the options of the first `docker run` / `podman run` (or create) in a
// command line, up to the image: what comes after the image is the program inside the container
// and its own arguments (a `--memory=64m` there is not the container's limit). ok is false when
// the command has no docker/podman run.
func DockerRunOptions(command string) (opts []string, ok bool) {
	f := strings.Fields(command)
	for i, a := range f {
		base := strings.TrimSuffix(strings.ToLower(filepathBase(a)), ".exe")
		if base != "docker" && base != "podman" {
			continue
		}
		j := i + 1
		for j < len(f) && strings.HasPrefix(f[j], "-") { // global options before the verb
			j++
		}
		if j >= len(f) || (f[j] != "run" && f[j] != "create") {
			continue
		}
		for k := j + 1; k < len(f); k++ {
			a := f[k]
			if !strings.HasPrefix(a, "-") || a == "-" {
				return opts, true // the image
			}
			opts = append(opts, a)
			if dockerValueFlags[a] && k+1 < len(f) {
				opts = append(opts, f[k+1])
				k++
			}
		}
		return opts, true
	}
	return nil, false
}

// DockerDetached says whether a command's docker run starts its container detached (-d): the CLI
// would return at once while the work runs on, out of firefighter's sight.
func DockerDetached(command string) bool {
	opts, ok := DockerRunOptions(command)
	if !ok {
		return false
	}
	for i, a := range opts {
		if dockerValueFlags[a] {
			continue
		}
		if i > 0 && dockerValueFlags[opts[i-1]] {
			continue // a value, not an option
		}
		if a == "-d" || a == "--detach" || a == "--detach=true" {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2 && !strings.ContainsAny(a, "=") && strings.Contains(a, "d") {
			return true // combined short flags: -dit, -itd
		}
	}
	return false
}

// filepathBase is filepath.Base for both separators (a Windows path seen on Linux too).
func filepathBase(p string) string {
	return filepath.Base(strings.ReplaceAll(strings.Trim(p, `"`), `\`, "/"))
}
