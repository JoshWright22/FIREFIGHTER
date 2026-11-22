package daemon

// Job state, lease timing, and signal files.

import (
	"os"
	"path/filepath"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

type job struct {
	*policy.Ticket
	rootCreated int64
	clientAt    int64          // the client's start time (with ClientPID: one process, for good)
	human       bool           // the human created it: only the human (or --force) changes it
	pids        map[int]int64  // member pid -> start time (pid + start time = one process, for good)
	applied     map[int]string // pid -> cpu set applied
	cpuNow      float64        // CPUs in use
	ramGB       float64
	vramGB      float64
	gpuUtil     float64
	peakVRAM    float64
	peakRAM     float64
	charged     float64
	overrun     bool
	granted     time.Time
	pausedAt    time.Time // cooperative pause flag set
	pausedTotal time.Duration
	suspended   time.Time
	checkpoint  time.Time // checkpoint-and-exit requested
	termAt      time.Time
	killedAt    time.Time
	reason      string
	container   string
	receipt     *Receipt
	extended    float64
	price       float64 // locked at grant: bid price x congestion x surcharge
	adopted     bool    // auto-adopted: heavy work an agent started outside firefighter
	backfilled  bool    // started ahead of a GPU head waiting on VRAM: can't extend its lease
	device      int     // GPU index the job was placed on (-1: any/unknown)
	engine      string  // container engine of j.container: docker | podman
	// ctrSeen: a container only guessed to be this job's (created while its docker run was live, no
	// label, no --name match). It counts toward the job's RAM, but the guard never caps, pauses or
	// kills it: the human may have started it in that window (review 11/20).
	ctrSeen      string
	ownerAgent   int       // pid of the agent session that submitted it (0: a human)
	ownerCreated int64     // its start time (a reused pid isn't the same session)
	ackBusy      time.Time // acked the pause but still computing on the GPU since then
	// held: the guard suspended it to free a resource the human is short of ("ram" | "disk"); it
	// resumes where it stopped once there is room again. Nothing it computed is lost.
	held       string
	heldAt     time.Time
	heldDrive  string          // disk: the drive that ran low
	heldRAM    float64         // RAM it held when suspended: what it takes back on resume
	heldTotal  time.Duration   // all the time it has spent held
	holds      int             // times held: each one waits longer before resuming
	okSince    time.Time       // since when there has been room to resume it
	freedAt    time.Time       // suspended for RAM then: what that freed is measured a moment later
	freedGB    float64         // RAM its suspension (or its kill) gave back
	trimFailed bool            // suspending it did not free its RAM (locked or shared memory)
	cpuByPID   map[int]float64 // CPU seconds per member at the last reading
	written    float64         // GB its members have written so far
	writeRate  float64         // MB/s it writes, smoothed over ~10 s
	// For learning (usage histograms, one sample per run): time it really worked, and over that
	// time its CPU-seconds, GPU-percent-seconds and (network jobs) megabit-seconds.
	workSec, cpuSec, gpuSec float64
	netSum, netSec          float64
	written0                float64 // GB already written when first measured
	measured                bool
	oom                     bool // stopped for lack of RAM: its need is above the peak it reached
	vramOOM                 bool // it failed on its own with the GPU out of memory: its VRAM need is above what it reached
	failure                 *policy.Failure
}

func newJob(t *policy.Ticket) *job {
	j := &job{Ticket: t, pids: map[int]int64{}, applied: map[int]string{}, device: -1}
	if t.PID > 1 {
		j.rootCreated = sysinfo.Created(t.PID)
		if j.rootCreated != 0 {
			j.pids[t.PID] = j.rootCreated
		}
	}
	return j
}

func signalDir() string { return filepath.Join(config.Home(), "signals") }

// Signal files a job may watch (also passed to it as environment variables).
func PauseFile(id string) string      { return filepath.Join(signalDir(), id+".pause") }
func PausedFile(id string) string     { return filepath.Join(signalDir(), id+".paused") }
func CheckpointFile(id string) string { return filepath.Join(signalDir(), id+".checkpoint") }

// leaseUsed is how long a job has run, not counting time the guard held it paused.
func (j *job) leaseUsed(now time.Time) time.Duration {
	used := now.Sub(j.Started) - j.pausedTotal
	if !j.pausedAt.IsZero() {
		used -= now.Sub(j.pausedAt)
	}
	return used
}

func touch(p string) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p) // a link planted in signals/ is replaced, never written through
	}
	_ = os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)), 0o600)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// pastLease: a running job has used its whole lease (declared minutes plus extensions).
func (j *job) pastLease(now time.Time) bool {
	return j.Minutes > 0 && !j.Started.IsZero() && j.leaseUsed(now).Minutes() > j.Minutes+j.extended
}
