// Package client talks to the daemon, starting it first if it isn't running.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"net/url"

	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

type Client struct {
	ep   daemon.Endpoint
	http *http.Client
}

func read() (daemon.Endpoint, error) {
	var ep daemon.Endpoint
	b, err := os.ReadFile(daemon.EndpointPath())
	if err != nil {
		return ep, err
	}
	return ep, json.Unmarshal(b, &ep)
}

// Connect finds the daemon, starting it in the background when it's down.
func Connect() (*Client, error) {
	c := &Client{http: &http.Client{Timeout: 90 * time.Second}}
	if ep, err := read(); err == nil {
		c.ep = ep
		if c.ping() == nil {
			return c, nil
		}
	}
	if err := StartDaemon(); err != nil {
		return nil, err
	}
	for i := 0; i < 50; i++ {
		time.Sleep(200 * time.Millisecond)
		if ep, err := read(); err == nil {
			c.ep = ep
			if c.ping() == nil {
				return c, nil
			}
		}
	}
	return nil, fmt.Errorf("the firefighter daemon didn't start; see %s", daemon.EndpointPath())
}

// StartDaemon launches `firefighter daemon` detached from this process and its console.
func StartDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, "daemon")
		sysinfo.Detach(cmd, breakaway)
		if err = cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return nil
		}
	}
	return err
}

func (c *Client) ping() error {
	var out map[string]any
	return c.do("GET", "/v1/ping", nil, &out)
}

func (c *Client) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", c.ep.Port, path), r)
	if err != nil {
		return err
	}
	req.Header.Set("X-Firefighter-Token", c.ep.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

var ErrNotFound = fmt.Errorf("not found")

func (c *Client) Create(req daemon.Request) (daemon.View, error) {
	var v daemon.View
	return v, c.do("POST", "/v1/tickets", req, &v)
}

// Get long-polls up to wait seconds for the ticket to leave state.
func (c *Client) Get(id, state string, wait int) (daemon.View, error) {
	var v daemon.View
	return v, c.do("GET", fmt.Sprintf("/v1/tickets/%s?state=%s&wait=%d", id, state, wait), nil, &v)
}

func (c *Client) Act(id, verb string, a daemon.Act) (daemon.View, error) {
	var v daemon.View
	return v, c.do("POST", "/v1/tickets/"+id+"/"+verb, a, &v)
}

func (c *Client) Status() (daemon.Status, error) {
	var s daemon.Status
	return s, c.do("GET", "/v1/status", nil, &s)
}

// Quote asks the market maker what a job would cost and when it would start at each bid.
func (c *Client) Quote(gpu bool, minutes, vram, ram float64, project, cmd string) (policy.Quote, error) {
	var q policy.Quote
	return q, c.do("GET", fmt.Sprintf("/v1/quote?gpu=%t&minutes=%g&vram=%g&ram=%g&project=%s&cmd=%s", gpu, minutes, vram, ram, url.QueryEscape(project), cmd), nil, &q)
}

func (c *Client) Plan(p daemon.Plan) (daemon.Plan, error) {
	var out daemon.Plan
	return out, c.do("POST", "/v1/plans", p, &out)
}

func (c *Client) Unplan(id string) error { return c.do("DELETE", "/v1/plans/"+id, nil, nil) }

// Connect0 finds a running daemon without starting one.
func Connect0() (*Client, error) {
	ep, err := read()
	if err != nil {
		return nil, err
	}
	c := &Client{ep: ep, http: &http.Client{Timeout: 3 * time.Second}}
	if err := c.ping(); err != nil {
		return nil, err
	}
	return c, nil
}

// GetSoon is Get for a job that may not have registered yet (a detached run starting up): it
// retries "not found" for a few seconds while the job's log file exists.
func (c *Client) GetSoon(id string) (daemon.View, error) {
	end := time.Now().Add(15 * time.Second)
	for {
		v, err := c.Get(id, "", 0)
		if err != ErrNotFound || time.Now().After(end) {
			return v, err
		}
		if _, serr := os.Stat(LogPath(id)); serr != nil {
			return v, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
