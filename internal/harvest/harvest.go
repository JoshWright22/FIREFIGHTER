// Package harvest keeps the books for band 4, the idle harvester (Salad, BOINC, ...): what it
// costs in electricity by the hour, what it earns, and whether it may run at all.
//
// The rule: the harvester runs only while it pays. Before each start, and every minute while it
// runs, the expected earnings per hour (after the payout cut) must beat the expected power cost
// per hour at this hour's price; peak hours that would lose money are never harvested. And once
// enough harvested hours are covered by reported earnings, a negative trailing net (earned minus
// cut minus power) trips it off until the human resets it.
//
// Files in the state folder, all plain text the human can read or edit:
//
//	harvest-ledger.csv    one row per clock hour the harvester ran: minutes, watts, kWh, price, cost
//	harvest-earnings.csv  what it earned: one row per report (firefighter harvest earned 3.20)
//	harvest-trip.txt      present while the guard has tripped it off (why); delete it to reset
package harvest

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Period is a time-of-use price: it applies in the listed months, on the listed weekdays, from
// From to To (local time, "HH:MM", To exclusive; From > To wraps past midnight).
type Period struct {
	Months string  `toml:"months"` // "5-10", "1-4,11-12"; empty = every month
	Days   string  `toml:"days"`   // "mon-fri", "sat,sun"; empty = every day
	From   string  `toml:"from"`
	To     string  `toml:"to"`
	Cents  float64 `toml:"cents"` // per kWh, all-in
}

// Profit is the guard's configuration ([harvester.profit] in firefighter.toml). It is off until a
// price is set.
type Profit struct {
	CentsPerKWh float64  `toml:"cents_per_kwh"` // all-in price outside any period; 0 = guard off
	Periods     []Period `toml:"periods"`       // time-of-use prices; the first match wins
	// EarnPerHour: expected USD per harvested hour (before the cut), used until reported earnings
	// cover MinHours of harvesting; then the measured rate replaces it.
	EarnPerHour float64 `toml:"earn_per_hour"`
	CutPct      float64 `toml:"cut_pct"`        // taken at cash-out
	MinNetHour  float64 `toml:"min_net_hour"`   // USD/h the harvester must clear to start (0 = break-even)
	PSUEff      float64 `toml:"psu_efficiency"` // DC watts measured / this = watts from the wall
	CPUWatts    float64 `toml:"cpu_watts"`      // per logical CPU the harvester keeps busy
	ExtraWatts  float64 `toml:"extra_watts"`    // the rest it adds (RAM, fans, disk), DC
	// IdleWatts: the whole PC at idle, charged only when it would otherwise sleep (set it if the
	// harvester is what keeps the machine awake; 0 when the PC stays on anyway).
	IdleWatts    float64 `toml:"idle_watts"`
	TrailingDays float64 `toml:"trailing_days"` // window of the trailing net
	MinHours     float64 `toml:"min_hours"`     // harvested hours covered by earnings before the net can trip it
	StaleDays    float64 `toml:"stale_days"`    // harvested hours this old with no earnings reported: hold off
}

func (p Profit) On() bool { return p.CentsPerKWh > 0 }

// Defaults fills unset values.
func (p *Profit) Defaults() {
	if p.PSUEff <= 0 || p.PSUEff > 1 {
		p.PSUEff = 0.88
	}
	if p.TrailingDays <= 0 {
		p.TrailingDays = 14
	}
	if p.MinHours <= 0 {
		p.MinHours = 24
	}
	if p.StaleDays <= 0 {
		p.StaleDays = 9
	}
}

// Price is the all-in price in cents per kWh at t.
func (p Profit) Price(t time.Time) float64 {
	for _, r := range p.Periods {
		if r.matches(t) {
			return r.Cents
		}
	}
	return p.CentsPerKWh
}

func (r Period) matches(t time.Time) bool {
	if r.Months != "" && !inList(r.Months, int(t.Month()), monthNum) {
		return false
	}
	if r.Days != "" && !inList(r.Days, int(t.Weekday()), dayNum) {
		return false
	}
	from, ok1 := clock(r.From)
	to, ok2 := clock(r.To)
	if !ok1 || !ok2 {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if from <= to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

func clock(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, false
	}
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if e1 != nil || e2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

var days = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func dayNum(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for i, d := range days {
		if strings.HasPrefix(s, d) {
			return i, true
		}
	}
	return 0, false
}

func monthNum(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil && n >= 1 && n <= 12
}

// inList says whether v is in a list like "1-4,11-12" or "mon-fri,sun" (ranges may wrap).
func inList(list string, v int, parse func(string) (int, bool)) bool {
	for _, part := range strings.Split(list, ",") {
		a, b, isRange := strings.Cut(part, "-")
		lo, ok := parse(a)
		if !ok {
			continue
		}
		hi := lo
		if isRange {
			if hi, ok = parse(b); !ok {
				continue
			}
		}
		if (lo <= hi && v >= lo && v <= hi) || (lo > hi && (v >= lo || v <= hi)) {
			return true
		}
	}
	return false
}

// Hour is one ledger row: a clock hour in which the harvester ran.
type Hour struct {
	Start   time.Time
	OnSec   float64
	WattSec float64 // wall watt-seconds
	GPUWSec float64 // GPU board watt-seconds (measured)
	Cents   float64 // price that hour (cents/kWh; the last one seen)
	CostUSD float64
}

func (h Hour) KWh() float64 { return h.WattSec / 3.6e6 }

// Earning is one report of money earned, covering harvesting up to Through.
type Earning struct {
	Through time.Time
	USD     float64
	Note    string
}

// Book is the ledger and the earnings, in memory.
type Book struct {
	Dir      string
	Hours    []Hour
	Earnings []Earning
	earnMod  time.Time
	dirty    bool
}

func (b *Book) ledgerPath() string   { return filepath.Join(b.Dir, "harvest-ledger.csv") }
func EarningsPath(dir string) string { return filepath.Join(dir, "harvest-earnings.csv") }
func TripPath(dir string) string     { return filepath.Join(dir, "harvest-trip.txt") }

const stamp = "2006-01-02 15:04"

var ledgerHead = []string{"hour", "minutes_on", "avg_wall_watts", "avg_gpu_watts", "kwh", "cents_per_kwh", "cost_usd"}

// Open loads the ledger and the earnings from dir.
func Open(dir string) *Book {
	b := &Book{Dir: dir}
	if rows := readCSV(b.ledgerPath()); len(rows) > 1 {
		for _, r := range rows[1:] {
			if len(r) < 7 {
				continue
			}
			t, err := time.ParseInLocation(stamp, r[0], time.Local)
			if err != nil {
				continue
			}
			min, w, g, c, cost := f(r[1]), f(r[2]), f(r[3]), f(r[5]), f(r[6])
			b.Hours = append(b.Hours, Hour{Start: t, OnSec: min * 60, WattSec: w * min * 60, GPUWSec: g * min * 60, Cents: c, CostUSD: cost})
		}
	}
	b.LoadEarnings()
	return b
}

// LoadEarnings rereads harvest-earnings.csv if it changed (the human edits it, or the CLI appends).
func (b *Book) LoadEarnings() {
	st, err := os.Stat(EarningsPath(b.Dir))
	if err != nil {
		b.Earnings, b.earnMod = nil, time.Time{}
		return
	}
	if st.ModTime().Equal(b.earnMod) {
		return
	}
	b.earnMod = st.ModTime()
	b.Earnings = ReadEarnings(b.Dir)
}

// ReadEarnings parses harvest-earnings.csv: through (YYYY-MM-DD or YYYY-MM-DD HH:MM), usd, note.
func ReadEarnings(dir string) []Earning {
	var out []Earning
	for i, r := range readCSV(EarningsPath(dir)) {
		if len(r) < 2 || (i == 0 && strings.EqualFold(strings.TrimSpace(r[0]), "through")) {
			continue
		}
		t, err := parseWhen(r[0])
		if err != nil {
			continue
		}
		e := Earning{Through: t, USD: f(r[1])}
		if len(r) > 2 {
			e.Note = r[2]
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Through.Before(out[j].Through) })
	return out
}

func parseWhen(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.ParseInLocation(stamp, s, time.Local); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return t, err
	}
	return t.Add(24*time.Hour - time.Minute), nil // a date covers that whole day
}

// AddEarning appends a report to harvest-earnings.csv.
func AddEarning(dir string, e Earning) error {
	p := EarningsPath(dir)
	_, err := os.Stat(p)
	fresh := os.IsNotExist(err)
	fh, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	if fresh {
		_ = w.Write([]string{"through", "usd", "note"})
	}
	_ = w.Write([]string{e.Through.Format(stamp), strconv.FormatFloat(e.USD, 'f', 4, 64), e.Note})
	w.Flush()
	return w.Error()
}

// Accrue adds dt of harvesting at wallW (GPU board gpuW) to the ledger, priced at cents/kWh.
func (b *Book) Accrue(now time.Time, dt time.Duration, wallW, gpuW, cents float64) {
	if dt <= 0 || dt > 5*time.Minute {
		return
	}
	hr := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	if n := len(b.Hours); n == 0 || !b.Hours[n-1].Start.Equal(hr) {
		b.Hours = append(b.Hours, Hour{Start: hr})
	}
	h := &b.Hours[len(b.Hours)-1]
	s := dt.Seconds()
	h.OnSec += s
	h.WattSec += wallW * s
	h.GPUWSec += gpuW * s
	h.Cents = cents
	h.CostUSD += wallW * s / 3.6e6 * cents / 100
	b.dirty = true
}

// Save rewrites harvest-ledger.csv if anything changed.
func (b *Book) Save() error {
	if !b.dirty {
		return nil
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write(ledgerHead)
	for _, h := range b.Hours {
		min := h.OnSec / 60
		avg := func(ws float64) string {
			if h.OnSec <= 0 {
				return "0"
			}
			return strconv.FormatFloat(ws/h.OnSec, 'f', 1, 64)
		}
		_ = w.Write([]string{h.Start.Format(stamp), strconv.FormatFloat(min, 'f', 2, 64), avg(h.WattSec), avg(h.GPUWSec),
			strconv.FormatFloat(h.KWh(), 'f', 4, 64), strconv.FormatFloat(h.Cents, 'f', 2, 64), strconv.FormatFloat(h.CostUSD, 'f', 5, 64)})
	}
	w.Flush()
	tmp := b.ledgerPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.ledgerPath()); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

// Totals sums the ledger and the earnings over [from, to).
type Totals struct {
	Hours, KWh, CostUSD, EarnedUSD, NetUSD float64
	CoveredHours, CoveredCost              float64 // harvesting up to the last earnings report in the window
	LastReport                             time.Time
}

func (b *Book) Totals(p Profit, from, to time.Time) Totals {
	var t Totals
	for _, e := range b.Earnings {
		if !e.Through.Before(from) && e.Through.Before(to) {
			t.EarnedUSD += e.USD
			if e.Through.After(t.LastReport) {
				t.LastReport = e.Through
			}
		}
	}
	for _, h := range b.Hours {
		if h.Start.Before(from) || !h.Start.Before(to) {
			continue
		}
		t.Hours += h.OnSec / 3600
		t.KWh += h.KWh()
		t.CostUSD += h.CostUSD
		if h.Start.Before(t.LastReport) {
			t.CoveredHours += h.OnSec / 3600
			t.CoveredCost += h.CostUSD
		}
	}
	t.NetUSD = t.EarnedUSD*(1-p.CutPct/100) - t.CoveredCost
	return t
}

// Check is the guard's verdict.
type Check struct {
	OK       bool
	Why      string
	Trip     bool    // the trailing net is negative: stay off until reset
	Cents    float64 // price now
	Watts    float64 // expected wall watts while harvesting
	EarnHour float64 // expected USD/h, after the cut
	CostHour float64 // expected USD/h of power now
	Measured bool    // EarnHour comes from reported earnings
	Trailing Totals
}

// Expected wall watts while harvesting: the ledger's average over the last 6 harvested hours,
// else fallback (the GPU's power limit, at worst).
func (b *Book) avgWatts(fallback float64) float64 {
	var ws, s float64
	for i := len(b.Hours) - 1; i >= 0 && s < 6*3600; i-- {
		ws += b.Hours[i].WattSec
		s += b.Hours[i].OnSec
	}
	if s < 600 {
		return fallback
	}
	return ws / s
}

// Decide says whether the harvester may run now. fallbackW is the wall power to assume before any
// has been measured.
func (b *Book) Decide(p Profit, now time.Time, fallbackW float64) Check {
	p.Defaults()
	c := Check{Cents: p.Price(now), Watts: b.avgWatts(fallbackW)}
	if p.IdleWatts > 0 {
		c.Watts += p.IdleWatts
	}
	c.CostHour = c.Watts / 1000 * c.Cents / 100
	tr := b.Totals(p, now.Add(-time.Duration(p.TrailingDays*24*float64(time.Hour))), now.Add(time.Minute))
	c.Trailing = tr
	keep := 1 - p.CutPct/100
	c.EarnHour = p.EarnPerHour * keep
	if tr.CoveredHours >= p.MinHours {
		c.Measured = true
		c.EarnHour = tr.EarnedUSD * keep / tr.CoveredHours
		if tr.NetUSD < 0 {
			c.Trip = true
			c.Why = fmt.Sprintf("trailing %.0f-day net is $%.2f (earned $%.2f, %.0f%% cut, power $%.2f over %.1f h)",
				p.TrailingDays, tr.NetUSD, tr.EarnedUSD, p.CutPct, tr.CoveredCost, tr.CoveredHours)
			return c
		}
	}
	// Harvested hours nobody has reported earnings for: hold off once the oldest is StaleDays old,
	// so a run with no money coming in can't go on unseen.
	var last time.Time
	if n := len(b.Earnings); n > 0 {
		last = b.Earnings[n-1].Through
	}
	for _, h := range b.Hours {
		if h.Start.After(last) && h.OnSec > 0 {
			if now.Sub(h.Start) > time.Duration(p.StaleDays*24*float64(time.Hour)) {
				c.Why = fmt.Sprintf("no earnings reported since %s (harvesting since then is unpaid as far as firefighter knows): run firefighter harvest earned <usd>",
					h.Start.Format("Jan 2"))
				return c
			}
			break
		}
	}
	net := c.EarnHour - c.CostHour
	if net < p.MinNetHour || c.EarnHour <= 0 {
		c.Why = fmt.Sprintf("would net $%.3f/h: earns $%.3f/h after the cut, power %.0f W at %.1f¢/kWh costs $%.3f/h",
			net, c.EarnHour, c.Watts, c.Cents, c.CostHour)
		return c
	}
	c.OK = true
	c.Why = fmt.Sprintf("nets $%.3f/h (earns $%.3f, power %.0f W at %.1f¢ = $%.3f)", net, c.EarnHour, c.Watts, c.Cents, c.CostHour)
	return c
}

// WallWatts is the harvester's draw from the wall: the GPU board power (measured), the CPUs its
// processes keep busy (measured) at cpuW each, and the rest, divided by the PSU's efficiency.
func WallWatts(p Profit, gpuW, cpusBusy float64) float64 {
	p.Defaults()
	return (gpuW + cpusBusy*p.CPUWatts + p.ExtraWatts) / p.PSUEff
}

func f(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$")), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func readCSV(path string) [][]string {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	r := csv.NewReader(fh)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, _ := r.ReadAll()
	return rows
}
