package daemon

import (
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review round 6 (Fable): sizes from docker inspect or a --memory flag are agent text.
func TestMemSizeRejectsNonsense(t *testing.T) {
	for in, want := range map[string]float64{"4g": 4, "512m": 0.5, "4gb": 4, "1073741824": 1, "": 0, "-4g": 0,
		"nan": 0, "NaNg": 0, "infg": 0, "+Inf": 0, "1e300g": 0, "2000000g": 0, "abc": 0} {
		got := memSize(in)
		if math.IsNaN(got) || math.Abs(got-want) > 1e-9 {
			t.Errorf("memSize(%q) = %v, want %v", in, got, want)
		}
	}
}

// A damaged state.json is set aside and the guard starts clean, instead of running on part of it.
func TestDamagedStateIsSetAside(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FIREFIGHTER_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"accounts": {"a": {"spent": 1}}, "history": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := load()
	if len(s.Accounts) != 0 || s.History == nil || s.Receipts == nil {
		t.Fatalf("partial state kept: %+v", s.Accounts)
	}
	bad, _ := filepath.Glob(filepath.Join(home, "state.json.bad-*"))
	if len(bad) != 1 {
		t.Fatalf("damaged file not kept aside: %v", bad)
	}
}

// A reply that can't be encoded is a 500, not a 200 with half a body.
func TestReplyEncodeErrorIs500(t *testing.T) {
	w := httptest.NewRecorder()
	reply(w, map[string]any{"x": math.NaN()})
	if w.Code != 500 {
		t.Fatalf("code %d, body %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	reply(w, map[string]int{"x": 1})
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"x":1}` {
		t.Fatalf("code %d, body %q", w.Code, w.Body.String())
	}
}
