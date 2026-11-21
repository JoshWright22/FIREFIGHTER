package daemon

// Local HTTP server, endpoint discovery, and caller identity.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// Endpoint is how clients find the daemon: written to <home>/daemon.json.
type Endpoint struct {
	PID     int    `json:"pid"`
	Created int64  `json:"created"` // the daemon's start time: a reused pid is never mistaken for it
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Version string `json:"version"`
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func EndpointPath() string { return filepath.Join(config.Home(), "daemon.json") }

// Serve listens on localhost, publishes the endpoint, and runs the control loop.
func (d *Daemon) Serve() error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	d.Token, d.Port = hex.EncodeToString(tok), ln.Addr().(*net.TCPAddr).Port
	logf("daemon up on 127.0.0.1:%d (pid %d)", d.Port, os.Getpid())
	d.mu.Lock()
	d.step(d.reader.Read(), nil, nil, time.Now(), 0) // measure before the first ticket arrives
	d.mu.Unlock()
	go d.Run()
	go d.autoUpdate()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, stopSignals...)
	go func() {
		<-sig // launchd/systemd stopping it, or Ctrl+C: same clean stop as the API
		logf("stop signal")
		d.Shutdown()
	}()
	b, _ := json.Marshal(Endpoint{PID: os.Getpid(), Created: sysinfo.Created(os.Getpid()), Port: d.Port, Token: d.Token, Version: d.Version})
	if err := writeAtomic(EndpointPath(), b, 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-d.stop
		_ = srv.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown stops the guard cleanly: anything it suspended is thawed, pause files go, and open jobs
// are saved so their clients can re-register with the next guard. Safe to call more than once.
func (d *Daemon) Shutdown() {
	if !d.stopping.CompareAndSwap(false, true) {
		return
	}
	d.mu.Lock()
	d.release()
	d.st.Open = d.openJobs(time.Now())
	d.saveState()
	d.mu.Unlock()
	close(d.stop)
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"ok": true, "pid": os.Getpid(), "version": d.Version})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true})
		logf("shutdown requested")
		go func() {
			time.Sleep(100 * time.Millisecond)
			d.Shutdown()
		}()
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		reply(w, d.status())
	})
	mux.HandleFunc("POST /v1/tickets", d.create)
	mux.HandleFunc("GET /v1/quote", d.quoteHandler)
	mux.HandleFunc("GET /v1/tickets/{id}", d.get)
	mux.HandleFunc("POST /v1/tickets/{id}/{verb}", d.act)
	mux.HandleFunc("POST /v1/plans", d.plan)
	mux.HandleFunc("DELETE /v1/plans/{id}", d.unplan)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only local clients by name (defense in depth against DNS rebinding; the token already
		// can't be sent by a page without a CORS preflight, which fails).
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			h = strings.Trim(r.Host, "[]") // no port given
		}
		if h != "127.0.0.1" && h != "localhost" && h != "::1" {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Firefighter-Token") != d.Token {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v) // whole reply first: a half-written body would read as a truncated success
	if err != nil {
		logf("reply: %v", err)
		fail(w, http.StatusInternalServerError, "couldn't encode the reply")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

func fail(w http.ResponseWriter, code int, msg string) { http.Error(w, msg, code) }

// cleanText drops control characters (newlines, terminal escapes) and trims to max runes: text one
// agent sends is shown to other agents and printed in the human's terminal, so it must not forge
// lines (a fake "do:") or carry escape sequences.
func cleanText(s string, max int) string {
	s = Defang(s) // before cutting: "do:" -> "do -" is one longer, and the result must stay within max (found by fuzzing)
	var b strings.Builder
	n := 0
	for _, r := range s {
		if sysinfo.Printable(string(r)) == "" {
			r = ' '
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return Defang(strings.TrimSpace(b.String())) // again: removing control characters can join "do" and ":"
}

var doRe = regexp.MustCompile(`(?i)\bdo\s*:`)

// Defang keeps agent-written text (names, plans, notes) from posing as firefighter's own reply
// grammar when it is shown to other agents: replies end with "| do: <next step>", and the server
// tells agents to follow it. A "|" becomes "/" and "do:" becomes "do -" (review 11/20, OWASP LLM01).
func Defang(s string) string {
	return doRe.ReplaceAllString(strings.ReplaceAll(s, "|", "/"), "do -")
}

// callerPID finds the local process on the other end of a request's TCP connection (0 if unknown).
// It matches the whole established connection (the client's address and port, and ours), so a
// process can't pose as another by binding a port that one listens on. -1: the lookup itself isn't
// available here (callers fall back); 0: no such connection (the request is refused).
func callerPID(r *http.Request) int {
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() { // all of 127/8 and ::1
		return 0
	}
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return -1
	}
	lhost, lport, err := net.SplitHostPort(local.String())
	if err != nil {
		return -1
	}
	p, _ := strconv.Atoi(port)
	lp, _ := strconv.Atoi(lport)
	conns, err := psnet.Connections("tcp")
	if err != nil || len(conns) == 0 {
		time.Sleep(50 * time.Millisecond) // one retry: a transient failure shouldn't refuse a job
		if conns, err = psnet.Connections("tcp"); err != nil || len(conns) == 0 {
			return -1
		}
	}
	for _, c := range conns {
		if c.Pid > 0 && c.Pid != int32(os.Getpid()) && strings.EqualFold(c.Status, "ESTABLISHED") &&
			int(c.Laddr.Port) == p && sameIP(c.Laddr.IP, host) && int(c.Raddr.Port) == lp && sameIP(c.Raddr.IP, lhost) {
			return int(c.Pid)
		}
	}
	return 0
}

func sameIP(a, b string) bool {
	x, y := net.ParseIP(a), net.ParseIP(b)
	return x != nil && y != nil && x.Equal(y)
}

// sessionOf names the agent session a caller belongs to: the nearest agent CLI above it, else (when
// the caller isn't a provable human session) the nearest agent host such as node or an IDE (one
// session per host process), else the caller itself. human: a terminal or the desktop started it,
// with no agent in between.
func (d *Daemon) sessionOf(caller int) (session int, human bool) {
	if caller <= 1 {
		return 0, false
	}
	g := d.cfgShared.Load().Guard // outside the lock: never the field the loop swaps
	if a := sysinfo.AgentAncestorPID(caller, g.AgentParents); a != 0 {
		return a, false
	}
	if sysinfo.HumanSession(caller, g.HumanHosts, g.AgentParents, g.AgentHosts) {
		return 0, true
	}
	if h := sysinfo.AgentAncestorPID(caller, g.AgentHosts); h != 0 {
		return h, false
	}
	return caller, false
}
