//go:build linux

package nputurbo

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/metrics"
)

// Config holds the controller tunables (mapped from config.NputurboConfig by
// the caller in cmd/catmonitor).
type Config struct {
	Interval          time.Duration
	StragglerURL      string
	StragglerTimeout  time.Duration
	NpuTurboBin       string // npu_turbo binary (global above-rated raise)
	NpuTurboTimeout   time.Duration
	StepMhz           int
	DryRun            bool
	RestoreOnShutdown bool
	Logger            *slog.Logger
}

// Storage is the subset of the storage interface the controller needs (write
// state metrics for observability).
type Storage interface {
	Write(ms []collector.Metric) error
}

// StragglerSource is the seam for the straggler HTTP source (real =
// straggler.Default(); tests inject a fake). Fetch returns the slow-card
// detection result body; parsing stays in ParseSlowCards.
type StragglerSource interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Controller is the nputurbo control loop. Stateless model: each tick it
// fetches the straggler slow-device result over HTTP, reads per-device
// current + rated frequencies from the FreqProvider (snapshot_npu.json),
// computes target freqs B = roundStep(min(A*score, M)) with A = aicore_freq
// and M from the static rated→max table, then restores ALL devices to rated
// and re-applies the boost set from the fresh list: above-rated targets in
// one batch (global raise via the npu_turbo tool + lower the others), then
// at-or-below-rated targets pinned individually. No cross-tick state.
type Controller struct {
	cfg      Config
	stragg   StragglerSource
	actuator *Actuator
	freqs    FreqProvider
	store    Storage
	logger   *slog.Logger
}

func NewController(cfg Config, stragg StragglerSource, actuator *Actuator, freqs FreqProvider, store Storage) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if freqs == nil {
		freqs = noopFreqProvider{}
	}
	return &Controller{cfg: cfg, stragg: stragg, actuator: actuator, freqs: freqs, store: store, logger: cfg.Logger}
}

// Run is the control loop. Ticks at cfg.Interval until ctx is cancelled.
// Shutdown (main.go) cancels ctx then calls Restore() for best-effort
// frequency recovery.
func (c *Controller) Run(ctx context.Context) {
	if c.cfg.Interval <= 0 {
		c.cfg.Interval = 60 * time.Second
	}
	c.logger.Info(fmt.Sprintf("nputurbo controller started (interval %s, dry_run %v, straggler_url %s)",
		c.cfg.Interval, c.cfg.DryRun, c.cfg.StragglerURL))
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(time.Now())
		}
	}
}

// Restore is the best-effort shutdown hook: restore all devices to rated.
func (c *Controller) Restore() {
	if !c.cfg.RestoreOnShutdown {
		return
	}
	if err := c.actuator.CleanAll(); err != nil {
		c.logger.Error("nputurbo: shutdown restore failed", "error", err)
	}
}

// tick is exported for tests; it runs one control cycle. Stateless: restore
// everything, then apply the fresh boost list (above-rated batch first, then
// per-device pins — the order matters because the above-rated batch's
// lower-others step would overwrite earlier per-device pins).
func (c *Controller) tick(now time.Time) {
	// planBoosts runs straggler + parse + per-device A/M lookup (no
	// actuation). On failure it returns err → complete no-op: we have no
	// fresh list to act on, so nothing is cleaned or boosted either.
	p, err := c.planBoosts()
	if err != nil {
		c.logger.Error("nputurbo: cannot build boost plan this cycle; no boost and no restore", "error", err)
		return
	}
	c.logger.Info(fmt.Sprintf("nputurbo: %d slow device(s) reported, %d to boost, %d skipped",
		len(p.cards), len(p.rows), len(p.skipped)))
	if c.cfg.DryRun {
		for _, r := range p.rows {
			c.logger.Info(fmt.Sprintf("nputurbo dry-run: would boost device %d from %d to %d MHz (score %.2f, rated %d, cap %d)",
				r.ID, r.A, r.B, r.Score, r.Rated, r.M))
		}
		c.emitMetrics(now, len(p.rows))
		return
	}
	// 1. Restore every device to its own rated frequency. This resets any
	// previous boost before the fresh list is applied, so recovered devices
	// simply stay at rated. A failure is logged but does not stop the boosts.
	if err := c.actuator.CleanAll(); err != nil {
		c.logger.Error("nputurbo: failed to restore all devices to rated; continuing with boosts", "error", err)
	}
	// 2. Above-rated targets: one batch — global raise to maxB via the
	// npu_turbo tool, then lower every non-target to its own rated. Must run
	// BEFORE the at-or-below pins (the lower step would overwrite them). All
	// above-rated B values equal M under the current rated→max map (the cap
	// is one step above rated), so a single raise frequency covers the group.
	var aboveIDs []int
	maxAbove := 0
	var belowRows []BoostRow
	for _, r := range p.rows {
		if r.B > r.Rated {
			aboveIDs = append(aboveIDs, r.ID)
			if r.B > maxAbove {
				maxAbove = r.B
			}
		} else {
			belowRows = append(belowRows, r)
		}
	}
	if len(aboveIDs) > 0 {
		rctx, rcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
		err := c.actuator.BoostAbove(rctx, aboveIDs, maxAbove)
		rcancel()
		if err != nil {
			c.logger.Error("nputurbo: failed to boost above-rated device(s)",
				"ids", aboveIDs, "target_mhz", maxAbove, "error", err)
		}
	}
	// 3. At-or-below-rated targets (ramp stage): pin each individually via
	// native DVFS (no side effects on other devices).
	for _, r := range belowRows {
		if err := c.actuator.BoostAtOrBelow(r.ID, r.B); err != nil {
			c.logger.Error("nputurbo: failed to pin device", "id", r.ID, "target_mhz", r.B, "error", err)
		}
	}
	c.logger.Info(fmt.Sprintf("nputurbo: cycle done — restored all devices, boosted %d above-rated device(s), pinned %d at-or-below-rated device(s)",
		len(aboveIDs), len(belowRows)))
	c.emitMetrics(now, len(p.rows))
}

// plan is one control cycle's read-only computation (no actuation).
type plan struct {
	cards   []SlowCard
	rows    []BoostRow
	skipped []SkipRow
}

// planBoosts runs straggler fetch + parse + per-device A/M lookup + compute
// B. It does NOT actuate. A is the device's live aicore_freq from the
// FreqProvider (snapshot_npu.json); M is looked up from aicore_rated_freq via
// the static rated→max table. A listed device lands in rows (with B) when
// score>1 and its frequencies are processable; anything else is reported in
// plan.skipped with a reason (score<=1 → treated as not listed; missing freq
// data; unmapped rating; current above cap — never downclock). A slow-device
// list with entries but an empty freq map is an error (caller no-ops the
// whole tick — same philosophy as a straggler failure: no input, no
// actuation). Shared by tick (which then actuates) and RunOnce (CLI preview,
// which does not).
func (c *Controller) planBoosts() (plan, error) {
	sctx, scancel := context.WithTimeout(context.Background(), c.stragglerTimeout())
	defer scancel()
	data, ferr := c.stragg.Fetch(sctx, c.cfg.StragglerURL)
	if ferr != nil {
		return plan{}, fmt.Errorf("straggler fetch: %w", ferr)
	}
	cards, perr := ParseSlowCards(data)
	if perr != nil {
		return plan{}, fmt.Errorf("parse: %w", perr)
	}
	var p plan
	p.cards = cards
	if len(cards) == 0 {
		return p, nil
	}
	freqs := c.freqs.DeviceFreqs()
	if len(freqs) == 0 {
		return plan{}, fmt.Errorf("no NPU freq data in snapshot (snapshot_npu.json missing/empty — check snapshot.enabled and the features scope)")
	}
	for _, sc := range cards {
		// score<=1 in the list is equivalent to not being listed at all —
		// the device is not slow; after the per-cycle restore it simply
		// stays at rated.
		if sc.Score <= 1.0 {
			p.skipped = append(p.skipped, SkipRow{ID: sc.ID, Score: sc.Score,
				Reason: fmt.Sprintf("score %.2f ≤ 1 (not slow, treated as not listed)", sc.Score)})
			continue
		}
		f, ok := freqs[sc.ID]
		if !ok || f.Current <= 0 || f.Rated <= 0 {
			c.logger.Warn(fmt.Sprintf("nputurbo: device %d has no frequency data in snapshot; skipped", sc.ID))
			p.skipped = append(p.skipped, SkipRow{ID: sc.ID, Score: sc.Score, Reason: "no frequency data in snapshot"})
			continue
		}
		m, okM := MaxBoostForRated(f.Rated)
		if !okM {
			c.logger.Warn(fmt.Sprintf("nputurbo: device %d rated %d MHz is not in the boost cap map; skipped", sc.ID, f.Rated))
			p.skipped = append(p.skipped, SkipRow{ID: sc.ID, Score: sc.Score,
				Reason: fmt.Sprintf("rated %d MHz is not in the boost cap map", f.Rated)})
			continue
		}
		if f.Current > m {
			// Current frequency already above the cap: never downclock, leave
			// the device alone (the per-cycle restore resets it to rated, but
			// it is never boosted above that).
			c.logger.Warn(fmt.Sprintf("nputurbo: device %d current %d MHz is already above the boost cap %d MHz; left untouched", sc.ID, f.Current, m))
			p.skipped = append(p.skipped, SkipRow{ID: sc.ID, Score: sc.Score,
				Reason: fmt.Sprintf("current %d MHz is already above the boost cap %d MHz", f.Current, m)})
			continue
		}
		b, _ := ComputeTargetB(f.Current, sc.Score, m, c.cfg.StepMhz)
		p.rows = append(p.rows, BoostRow{ID: sc.ID, A: f.Current, Rated: f.Rated, M: m, B: b, Score: sc.Score})
	}
	return p, nil
}

func (c *Controller) stragglerTimeout() time.Duration {
	if c.cfg.StragglerTimeout > 0 {
		return c.cfg.StragglerTimeout
	}
	return 60 * time.Second
}

func (c *Controller) npuTurboTimeout() time.Duration {
	if c.cfg.NpuTurboTimeout > 0 {
		return c.cfg.NpuTurboTimeout
	}
	return 120 * time.Second
}

// emitMetrics builds nputurbo.* state metrics, applies the catalog filter,
// and writes them to storage for observability. boostedCount is the number
// of devices boosted this cycle (the stateless model has no cross-tick
// boosted set).
func (c *Controller) emitMetrics(now time.Time, boostedCount int) {
	if c.store == nil {
		return
	}
	active := 0.0
	if boostedCount > 0 {
		active = 1
	}
	ok := 0.0
	if c.actuator.Ok() {
		ok = 1
	}
	ms := []collector.Metric{
		{Component: "nputurbo", Name: "boost_active", Value: active, Unit: "", Timestamp: now},
		{Component: "nputurbo", Name: "boost_count", Value: float64(boostedCount), Unit: "", Timestamp: now},
		{Component: "nputurbo", Name: "actuator_ok", Value: ok, Unit: "", Timestamp: now},
	}
	filtered := metrics.Filter(ms)
	if len(filtered) > 0 {
		if err := c.store.Write(filtered); err != nil {
			c.logger.Error("nputurbo: failed to write nputurbo metrics", "error", err)
		}
	}
}
