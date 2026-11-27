package mcp

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/daemon"
)

// ea42b3cb (11/27): firefighter_status returned about 97 KB (the forecast alone 89 KB), over MCP
// clients' size limit, so agents got "result exceeds maximum allowed tokens" instead of the status
// and its do:. The tool's structured result leaves the forecast out and keeps the newest three
// receipts.
func TestStatusToolResultStaysSmall(t *testing.T) {
	st := daemon.Status{Forecast: &daemon.Forecast{}}
	for i := 0; i < 400; i++ { // as many learned commands as this PC has, each with its numbers
		st.Forecast.Commands = append(st.Forecast.Commands, daemon.CommandForecast{Project: "nanoTSFM", Name: fmt.Sprintf("nanoTSFM run %d with a long descriptive name", i)})
	}
	now := time.Now()
	for i := 0; i < 30; i++ {
		st.Recent = append(st.Recent, &daemon.Receipt{ID: fmt.Sprintf("%08x", i), Name: "job", Ended: now.Add(-time.Duration(i) * time.Minute)})
	}
	if b, _ := json.Marshal(st); len(b) < 40_000 {
		t.Fatalf("setup: the full status should be large, got %d bytes", len(b))
	}
	slim := slimStatus(st)
	b, _ := json.Marshal(slim)
	if len(b) > 8_000 || slim.Forecast != nil {
		t.Fatalf("the tool's status is %d bytes (forecast kept: %v), want under 8 KB without it", len(b), slim.Forecast != nil)
	}
	if len(slim.Recent) != 3 || slim.Recent[0].ID != "00000000" || slim.Recent[2].ID != "00000002" {
		t.Fatalf("want the newest three receipts, newest first: %+v", slim.Recent)
	}
	if st.Forecast == nil || len(st.Recent) != 30 {
		t.Fatal("slimStatus must not change the status it was given")
	}
}
