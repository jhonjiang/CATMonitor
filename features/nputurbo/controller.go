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
	NpuTurboCmd       string
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

// Controller is the nputurbo control loop. Each tick it fetches the straggler
// slow-device result over HTTP (GET straggler_url), reads per-device current +
// rated frequencies from the FreqProvider (snapshot_npu.json), computes target
// freqs B = roundStep(min(A*score, M)) with A = aicore_freq and M looked up
// from aicore_rated_freq via the static rated→max table, reconciles the
// boosted set (restore disappeared, boost listed), and emits state metrics.
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

// Restore is the best-effort shutdown hook: restore all boosted cards.
func (c *Controller) Restore() {
	if !c.cfg.RestoreOnShutdown {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.actuator.RestoreAll(ctx); err != nil {
		c.logger.Error("nputurbo shutdown restore failed", "error", err)
	}
}

// tick is exported for tests; it runs one control cycle.
func (c *Controller) tick(now time.Time) {
	// planBoosts runs straggler + parse + per-device A/M lookup (no
	// actuation). On straggler/parse failure — or a completely missing
	// frequency input (freqs map empty: snapshot_npu.json unreadable) — it
	// returns err → complete no-op: we have no fresh list to reconcile
	// against, so the previously-boosted state is left untouched (not
	// restored).
	p, err := c.planBoosts()
	if err != nil {
		c.logger.Error("nputurbo: cannot build boost plan this cycle; no boost and no restore", "error", err)
		return
	}
	// Reconcile to desired state. desired = {id: target} for every listed
	// device with score>1 (and processable freq data). The target carries the
	// boost frequency B plus the device's rated freq, both passed through to
	// the inject command. Devices already at their target stay in desired —
	// ComputeTargetB no longer skips on B<=A — so a boosted device is only
	// "recovered" when it leaves that set.
	desired := make(map[int]injectTarget, len(p.rows))
	for _, r := range p.rows {
		desired[r.ID] = injectTarget{B: r.B, Rated: r.Rated}
	}
	c.logger.Info(fmt.Sprintf("nputurbo: %d slow device(s) reported, %d to boost, %d skipped",
		len(p.cards), len(desired), len(p.skipped)))
	if c.cfg.DryRun {
		for _, r := range p.rows {
			c.logger.Info(fmt.Sprintf("nputurbo dry-run: would boost device %d from %d to %d MHz (score %.2f, rated %d, cap %d)",
				r.ID, r.A, r.B, r.Score, r.Rated, r.M))
		}
		c.emitMetrics(now)
		return
	}
	current := c.actuator.LastAppliedMap()
	// A boosted device has recovered when it is no longer listed, or its
	// listed score is <= 1 (equivalent to not listed). Devices skipped for
	// other reasons (missing freq data / unmapped rating / A above cap) are
	// still slow — they never count as recovered. Since clean is
	// all-or-nothing, any recovery forces clean + full re-inject (still-slow
	// devices are re-boosted after clean). When no device recovered, only
	// inject new/changed devices (idempotent re-set; no flicker on stable
	// devices).
	listedScore := make(map[int]float64, len(p.cards))
	for _, sc := range p.cards {
		listedScore[sc.ID] = sc.Score
	}
	recoveredCount := 0
	for id := range current {
		score, listed := listedScore[id]
		if !listed || score <= 1.0 {
			recoveredCount++
		}
	}
	if recoveredCount > 0 && len(current) > 0 {
		c.logger.Info(fmt.Sprintf("nputurbo: %d boosted device(s) recovered — restored all devices to baseline, re-boosting %d still-slow device(s)",
			recoveredCount, len(desired)))
		rctx, rcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
		_ = c.actuator.RestoreAll(rctx) // actuator logs clean success/failure incl. output
		rcancel()
		for id, t := range desired {
			bctx, bcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
			_ = c.actuator.Boost(bctx, id, t.B, t.Rated) // actuator logs inject incl. output
			bcancel()
		}
	} else {
		injected := 0
		for id, t := range desired {
			if c.actuator.LastApplied(id) == t.B {
				continue // idempotent: already at target
			}
			bctx, bcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
			if err := c.actuator.Boost(bctx, id, t.B, t.Rated); err == nil {
				injected++
			} // actuator logs inject success/failure incl. output
			bcancel()
		}
		switch {
		case injected > 0:
			c.logger.Info(fmt.Sprintf("nputurbo: boosted %d new/changed device(s); %d already at target, untouched",
				injected, len(desired)-injected))
		case len(desired) > 0:
			c.logger.Info(fmt.Sprintf("nputurbo: %d boosted device(s) already at target frequency; nothing to do", len(desired)))
		}
	}
	c.emitMetrics(now)
}

// injectTarget is one device's desired boost state: the target frequency B
// plus the device's rated frequency, both passed through to the npu_turbo
// tool's inject command (-r {rated}).
type injectTarget struct {
	B     int // target freq (MHz)
	Rated int // aicore_rated_freq (MHz)
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
		// for a previously-boosted device this is the recovery signal
		// (clean), not a plan entry.
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
			// the device alone (also not a recovery signal — it is still
			// slow).
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
// and writes them to storage for observability.
func (c *Controller) emitMetrics(now time.Time) {
	if c.store == nil {
		return
	}
	boosted := c.actuator.BoostedIDs()
	active := 0.0
	if len(boosted) > 0 {
		active = 1
	}
	ok := 0.0
	if c.actuator.Ok() {
		ok = 1
	}
	ms := []collector.Metric{
		{Component: "nputurbo", Name: "boost_active", Value: active, Unit: "", Timestamp: now},
		{Component: "nputurbo", Name: "boost_count", Value: float64(len(boosted)), Unit: "", Timestamp: now},
		{Component: "nputurbo", Name: "actuator_ok", Value: ok, Unit: "", Timestamp: now},
	}
	filtered := metrics.Filter(ms)
	if len(filtered) > 0 {
		if err := c.store.Write(filtered); err != nil {
			c.logger.Error("nputurbo: failed to write nputurbo metrics", "error", err)
		}
	}
}
