//go:build linux

package nputurbo

import (
	"fmt"
	"strings"
)

// BoostRow is one card's computed boost plan (no actuation).
type BoostRow struct {
	ID         int
	A          int // current freq (MHz, read from snapshot)
	B          int // target freq (MHz, =round50(min(A*score,M)))
	Score      float64
	WouldBoost bool // false if score<=1 or gain wiped by step quantization
}

// Snapshot is the read-only plan returned by RunOnce for the CLI preview.
type Snapshot struct {
	Rows       []BoostRow
	DryRun     bool
	ActuatorOK bool
	PlanErr    error
}

// RunOnce is the `catmonitor nputurbo` one-shot: run straggler once, parse,
// read snapshot freqs, compute each card's target B, and return a printable
// plan. Forces DryRun=true; never actuates. For actuation, run the daemon
// with nputurbo.enabled: true + dry_run: false.
func RunOnce(cfg Config, stragg StragglerSource, actuator *Actuator) Snapshot {
	cfg.DryRun = true
	c := NewController(cfg, stragg, actuator, nil)
	_, rows, _, err := c.planBoosts()
	return Snapshot{Rows: rows, DryRun: true, ActuatorOK: actuator.Available(), PlanErr: err}
}

// FormatSnapshot renders the plan as the human-readable `catmonitor nputurbo`
// status block.
func FormatSnapshot(s Snapshot, cfg Config) string {
	var b strings.Builder
	fmt.Fprintln(&b, "CATMonitor nputurbo (read-only preview)")
	fmt.Fprintf(&b, "  dry_run:        %v\n", s.DryRun)
	fmt.Fprintf(&b, "  actuator_ok:    %v\n", s.ActuatorOK)
	fmt.Fprintf(&b, "  max_freq_mhz:   %d  step: %d\n", cfg.MaxFreqMhz, cfg.StepMhz)
	if s.PlanErr != nil {
		fmt.Fprintf(&b, "  plan_error:     %v\n", s.PlanErr)
		return b.String()
	}
	if len(s.Rows) == 0 {
		fmt.Fprintln(&b, "  (no slow cards listed)")
		return b.String()
	}
	for _, r := range s.Rows {
		fmt.Fprintf(&b, "  id=%d  A=%d  score=%.4f  → B=%d  would_boost=%v\n",
			r.ID, r.A, r.Score, r.B, r.WouldBoost)
	}
	return b.String()
}
