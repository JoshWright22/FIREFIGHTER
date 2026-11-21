package policy

import (
	"regexp"
	"strings"
)

// Failure is why a job that ended on its own (not stopped by firefighter) failed, whether running
// it again as-is can help, and what to change. Modeled on OpenPAI's job exit spec, which tags each
// exit with a type and a retry reaction (microsoft/pai src/k8s-job-exit-spec): an agent gets a next
// step instead of a bare exit code, and firefighter learns from resource failures.
type Failure struct {
	Kind  string `json:"kind"`  // vram | ram | disk | missing | network | docker | crash | unknown
	Retry string `json:"retry"` // same: run it again as-is may work | change: change something first | no: it won't work as-is
	Why   string `json:"why"`
	Fix   string `json:"fix,omitempty"`
}

type exitRule struct {
	kind, retry, why, fix string
	codes                 []int          // any of these exit codes (empty: any)
	re                    *regexp.Regexp // and/or this in the output's tail (nil: codes only)
}

// Order matters: the first rule that matches wins, most specific first.
var exitRules = []exitRule{
	{kind: "vram", retry: "change", why: "the GPU ran out of memory",
		fix: "declare more VRAM (--vram), or lower the batch size; firefighter raises its learned need for this command",
		re:  regexp.MustCompile(`(?i)CUDA out of memory|CUDA error: out of memory|OutOfMemoryError: CUDA|CUBLAS_STATUS_ALLOC_FAILED|cudaErrorMemoryAllocation|CUDA_ERROR_OUT_OF_MEMORY|hipErrorOutOfMemory|HIP out of memory|failed to allocate .* (device|GPU) memory|out of memory on device|ggml_backend_cuda_buffer_type_alloc_buffer`)},
	{kind: "disk", retry: "change", why: "the disk filled up",
		fix: "free space (firefighter storage free --gb N for your space), or write somewhere with room",
		re:  regexp.MustCompile(`(?i)No space left on device|ENOSPC|There is not enough space on the disk|disk quota exceeded`)},
	{kind: "ram", retry: "change", why: "it ran out of RAM",
		fix:   "declare more RAM (--ram), or load less at once; firefighter raises its learned need for this command",
		codes: []int{137, -9, 9, -1073741801}, // SIGKILL (the OOM killer), Windows STATUS_NO_MEMORY
		re:    regexp.MustCompile(`(?i)\bMemoryError\b|std::bad_alloc|Cannot allocate memory|OOMKilled|fatal error: runtime: out of memory|JavaScript heap out of memory|Out of memory: Killed process`)},
	{kind: "docker", retry: "same", why: "the container engine wasn't reachable",
		fix: "start Docker Desktop (or the engine) and run it again",
		re:  regexp.MustCompile(`(?i)Cannot connect to the Docker daemon|docker daemon is not running|error during connect|open //\./pipe/docker_engine`)},
	{kind: "network", retry: "same", why: "a network call failed",
		fix: "run it again (mark it --net so it takes a network turn); check the URL if it keeps failing",
		re:  regexp.MustCompile(`(?i)Temporary failure in name resolution|Name or service not known|getaddrinfo failed|Connection (reset|refused|timed out)|ConnectionResetError|ReadTimeout|i/o timeout|HTTP Error 5\d\d|\b(502|503|504) (Bad Gateway|Service Unavailable|Gateway Time-?out)|429 Too Many Requests|TLS handshake timeout`)},
	{kind: "missing", retry: "no", why: "a program or module it needs isn't installed",
		fix:   "install it in the job's environment (or activate the venv/conda env in the command) and run it again",
		codes: []int{127, 9009},
		re:    regexp.MustCompile(`(?i)ModuleNotFoundError|No module named|ImportError|command not found|is not recognized as an internal or external command|Cannot find module|executable file not found|No such file or directory: '[^']*python`)},
	{kind: "crash", retry: "no", why: "the program crashed",
		fix:   "a bug or a bad build, not a resource: read the log before running it again",
		codes: []int{139, -11, 134, -6, -1073741819, -1073740940, -1073740791}, // SIGSEGV, SIGABRT, access violation, heap corruption, stack buffer overrun
		re:    regexp.MustCompile(`(?i)Segmentation fault|core dumped|Access violation|illegal memory access|device-side assert`)},
}

// ClassifyExit names the likely cause of a failed exit from its code and the last part of its
// output (nil when it ended ok). The output wins over the code: an exit code alone is ambiguous.
func ClassifyExit(exit int, tail string) *Failure {
	if exit == 0 {
		return nil
	}
	tail = lastBytes(tail, 8<<10) // the end: an error the job logged and recovered from earlier isn't why it failed
	for _, r := range exitRules { // output first
		if r.re != nil && r.re.MatchString(tail) {
			return &Failure{Kind: r.kind, Retry: r.retry, Why: r.why, Fix: r.fix}
		}
	}
	for _, r := range exitRules { // then the code
		for _, c := range r.codes {
			if c == exit {
				return &Failure{Kind: r.kind, Retry: r.retry, Why: r.why + " (by its exit code)", Fix: r.fix}
			}
		}
	}
	return &Failure{Kind: "unknown", Retry: "no", Why: "it exited with an error",
		Fix: "read the log; running it again unchanged usually fails the same way"}
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}
