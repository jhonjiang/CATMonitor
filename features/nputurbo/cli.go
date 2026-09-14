//go:build linux

package nputurbo

import (
	"fmt"
	"sort"
	"strings"
)

// BoostRow is one device's computed boost plan (no actuation).
type BoostRow struct {
	ID     int
	A      int // current aicore_freq from snapshot (MHz)
	Rated  int // rated aicore_freq from snapshot (MHz)
	M      int // boost cap from the rated→max static map (MHz)
	B      int // target freq (MHz, =round50(min(A*score,M)))
	Score  float64
}

// SkipRow is a listed device excluded from the plan and why (its score is
// ≤1, or its frequency data is missing / unmapped / above the cap).
type SkipRow struct {
	ID     int
	Score  float64
	Reason string
}

// Snapshot is the read-only plan returned by RunOnce for the CLI preview.
type Snapshot struct {
	Rows       []BoostRow
	Skipped    []SkipRow
	DryRun     bool
	ActuatorOK bool
	PlanErr    error
}

// RunOnce is the `catmonitor nputurbo` one-shot: run straggler once, parse,
// look up each device's current + rated freq, and return a printable plan.
// Forces DryRun=true; never actuates. For actuation, run the daemon with
// nputurbo.enabled: true + dry_run: false.
func RunOnce(cfg Config, stragg StragglerSource, actuator *Actuator, freqs FreqProvider) Snapshot {
	cfg.DryRun = true
	c := NewController(cfg, stragg, actuator, freqs, nil)
	p, err := c.planBoosts()
	return Snapshot{Rows: p.rows, Skipped: p.skipped, DryRun: true, ActuatorOK: actuator.Available(), PlanErr: err}
}

// FormatSnapshot renders the plan as the human-readable `catmonitor nputurbo`
// status block.
func FormatSnapshot(s Snapshot, cfg Config) string {
	var b strings.Builder
	fmt.Fprintln(&b, "CATMonitor nputurbo (read-only preview — no frequencies are changed)")
	fmt.Fprintf(&b, "  actuator available: %v\n", s.ActuatorOK)
	fmt.Fprintf(&b, "  boost caps:         %s  (step %d MHz)\n", formatRatedMaxBoost(), cfg.StepMhz)
	if s.PlanErr != nil {
		fmt.Fprintf(&b, "  plan error:         %v\n", s.PlanErr)
		return b.String()
	}
	if len(s.Rows) == 0 && len(s.Skipped) == 0 {
		fmt.Fprintln(&b, "  (no slow devices reported)")
		return b.String()
	}
	for _, r := range s.Rows {
		fmt.Fprintf(&b, "  device %d: current %d MHz → target %d MHz  (score %.2f, rated %d, cap %d)\n",
			r.ID, r.A, r.B, r.Score, r.Rated, r.M)
	}
	for _, r := range s.Skipped {
		fmt.Fprintf(&b, "  device %d: skipped — %s\n", r.ID, r.Reason)
	}
	return b.String()
}

// formatRatedMaxBoost renders the static rated→max table as
// "rated 1800 MHz → max 1850 MHz, ..." (sorted by rating for stable output).
func formatRatedMaxBoost() string {
	ratings := make([]int, 0, len(ratedMaxBoost))
	for rated := range ratedMaxBoost {
		ratings = append(ratings, rated)
	}
	sort.Ints(ratings)
	parts := make([]string, 0, len(ratings))
	for _, rated := range ratings {
		parts = append(parts, fmt.Sprintf("rated %d MHz → max %d MHz", rated, ratedMaxBoost[rated]))
	}
	return strings.Join(parts, ", ")
}
