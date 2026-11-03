package main

// `firefighter dashboard`: the live picture in a browser. A page embedded in the binary, served on
// 127.0.0.1 only, polling /status.json, which this process fills from the daemon. The daemon's token
// never reaches the browser, and requests with any other Host header are refused (DNS rebinding).

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/ui"
)

//go:embed dash
var dashFS embed.FS

func dashboard(args []string) error {
	fl := flag.NewFlagSet("dashboard", flag.ExitOnError)
	port := fl.Int("port", 7787, "port on 127.0.0.1")
	noOpen := fl.Bool("no-open", false, "don't open a browser")
	_ = fl.Parse(args)

	c, err := client.Connect()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	page, _ := fs.Sub(dashFS, "dash")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(page)))
	mux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s, err := c.Status()
		if err != nil { // the daemon restarted (upgrade, crash): reconnect once
			if c2, e := client.Connect(); e == nil {
				c = c2
				s, err = c.Status()
			}
		}
		mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fmt.Errorf("port %d is taken (try --port): %w", *port, err)
	}
	addr := ln.Addr().String()
	allowed := map[string]bool{addr: true, fmt.Sprintf("localhost:%d", *port): true}
	url := "http://" + addr + "/"
	ui.Say(ui.Hot, "dashboard on %s (Ctrl+C to stop)", url)
	if !*noOpen {
		openBrowser(url)
	}
	return http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r)
	}))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
