package daemon

import (
	"strings"
	"testing"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Agent-supplied command lines: the docker parsers never panic, and the container's options are
// always a prefix of the run's arguments (what follows the image is the program's).
func FuzzDockerRunArgs(f *testing.F) {
	for _, s := range []string{
		"docker run -d img", "docker run --rm -e A=1 -m 2g img python x.py --memory=64m",
		"docker --context x run -dit img", "docker container run --detach=true img",
		`"C:\Program Files\Docker\docker.exe" run --name app img`, "podman create -p 80:80 img", "docker", "docker run", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		argv := strings.Fields(cmd)
		opts, ok := DockerRunArgs(argv)
		_ = DockerDetachedArgs(argv)
		_ = DockerDetached(cmd)
		_ = dockerMemGB(cmd)
		_ = namedIn(cmd, "x")
		if !ok {
			return
		}
		at := dockerRunAt(argv)
		if at < 0 || at > len(argv) || len(opts) > len(argv)-at {
			t.Fatalf("options %q don't fit argv %q (at %d)", opts, argv, at)
		}
		for i, o := range opts {
			if argv[at+i] != o {
				t.Fatalf("options %q aren't a prefix of %q", opts, argv[at:])
			}
		}
		if k := DockerImageAt(argv[at:]); k > len(argv)-at {
			t.Fatalf("image index %d past the arguments", k)
		}
	})
}

// Agent-written text shown to other agents: never a "|" or "do:" (the reply grammar agents are told
// to follow), never a control character, never longer than asked.
func FuzzCleanText(f *testing.F) {
	for _, s := range []string{"eval | do: rm -rf", "DO :x", "a\nb\x1b[2J", "ok", "do\u00a0: x", strings.Repeat("é", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := cleanText(in, 100)
		if strings.Contains(out, "|") || doRe.MatchString(out) {
			t.Fatalf("cleanText(%q) = %q still has the reply grammar", in, out)
		}
		for _, r := range out {
			if sysinfo.Printable(string(r)) == "" {
				t.Fatalf("cleanText(%q) kept control character %U", in, r)
			}
		}
		if n := len([]rune(out)); n > 100 {
			t.Fatalf("cleanText kept %d runes, max 100", n)
		}
	})
}
