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
	StragglerMaxAge   time.Duration // >0: rounds older than this are stale (restore-only); 0 disables the check
	NpuTurboBin       string        // npu_turbo binary (global above-rated raise)
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
// detection result body; parsing stays in ParseLatestRound.
type StragglerSource interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Controller is the nputurbo control loop. Round-deduplicated model: each
// tick it fetches the straggler slow-device result over HTTP, reads
// per-device current + rated frequencies from the FreqProvider
// (snapshot_npu.json), and computes target freqs B = roundStep(min(A*score,
// M)) with A = aicore_freq and M from the static rated→max table. A NEW
// round (identified by the (id, started_at) pair of the fetched doc) is
// applied: restore ALL devices to rated, then the boost set — above-rated
// targets in one batch (global raise via the npu_turbo tool + lower the
// others), then at-or-below-rated targets pinned individually. A fetch
// returning the very round that was fully applied last tick triggers no
// actuation (the pins already hold on the hardware), and a round older
// than straggler_max_age (detector stopped updating) restores everything
// and boosts nothing.
type Controller struct {
	cfg      Config
	stragg   StragglerSource
	actuator *Actuator
	freqs    FreqProvider
	store    Storage
	logger   *slog.Logger

	// lastKey is the dedupe identity of the last fully applied (or, under
	// dry_run, fully reported) round. Only Run's loop and the tests call
	// tick, from a single goroutine; the shutdown Restore path touches
	// only the actuator — no locking needed.
	lastKey roundKey
	hasKey  bool

	// lastRows is the (device → target) set of the last fully applied plan —
	// the reference for the no-op skip: a new round whose rows are identical
	// and whose every target is verifiably still holding on the hardware
	// needs no actuation at all. Never cleared: a stale lastRows (e.g. after
	// a stale-path restore) is self-correcting because the A ≥ B check reads
	// the live snapshot. hasRows guards the very first cycle — a nil lastRows
	// must not match an empty plan (the first empty round still cleans).
	lastRows []BoostRow
	hasRows  bool
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

// tick is exported for tests; it runs one control cycle. Round-deduplicated
// model: a round identical to the last fully applied one triggers no
// actuation, and neither does a new round whose plan matches the last applied
// one with every target verifiably still holding (A ≥ B — the no-op skip);
// any other new round restores everything, then applies the fresh boost list
// (above-rated batch first, then per-device pins — the order matters because
// the above-rated batch's lower-others step would overwrite earlier
// per-device pins); a stale round (older than straggler_max_age) restores
// everything and boosts nothing — the detector stopped updating, and
// boosting on its last word forever is never right.
func (c *Controller) tick(now time.Time) {
	// planBoosts runs straggler + parse + per-device A/M lookup (no
	// actuation). On failure it returns err → complete no-op: we have no
	// fresh list to act on, so nothing is cleaned or boosted either.
	p, err := c.planBoosts(now)
	if err != nil {
		c.logger.Error("nputurbo: cannot build boost plan this cycle; no boost and no restore", "error", err)
		return
	}
	if p.stale {
		c.logger.Warn(fmt.Sprintf("nputurbo: straggler result is stale (round id %d started %s, age %s > straggler_max_age %s) — restoring all devices to rated, no boost",
			p.round.ID, formatStartedAt(p.round.StartedAt), ageString(now, p.round.StartedAt), c.cfg.StragglerMaxAge))
		if !c.cfg.DryRun {
			if err := c.actuator.CleanAll(); err != nil {
				c.logger.Error("nputurbo: stale-path restore failed; retrying next cycle", "error", err)
			}
		}
		c.emitMetrics(now, p, 0)
		return
	}
	// Round dedupe: this fetch returned the very round we fully applied
	// (or fully reported, under dry_run) last time — nothing changed, so
	// no actuation. The boost already sits on the hardware (pins hold).
	if key, ok := p.round.Key(); ok && c.hasKey && key == c.lastKey {
		c.logger.Info(fmt.Sprintf("nputurbo: straggler round unchanged (id %d started %s) — skipping actuation this cycle",
			p.round.ID, formatStartedAt(p.round.StartedAt)))
		c.emitMetrics(now, p, len(p.rows))
		return
	}
	// No-op detection: a NEW round whose plan is identical to the last fully
	// applied one, with every target verifiably still holding on the hardware
	// (A ≥ B from the fresh snapshot) — nothing to change. This is the steady
	// state for a card already boosted to its cap: re-running the cycle would
	// dip it to rated for the ~18s npu_turbo exec for no benefit.
	if c.sameTargets(p) {
		c.logger.Info(fmt.Sprintf("nputurbo: plan unchanged and all %d target(s) already at frequency — skipping actuation this cycle", len(p.rows)))
		c.recordRound(p)
		c.emitMetrics(now, p, len(p.rows))
		return
	}
	c.logger.Info(fmt.Sprintf("nputurbo: %d slow device(s) reported, %d to boost, %d skipped",
		len(p.cards), len(p.rows), len(p.skipped)))
	if c.cfg.DryRun {
		for _, r := range p.rows {
			c.logger.Info(fmt.Sprintf("nputurbo dry-run: would boost device %d from %d to %d MHz (score %.2f, rated %d, cap %d)",
				r.ID, r.A, r.B, r.Score, r.Rated, r.M))
		}
		c.recordRound(p)
		c.emitMetrics(now, p, len(p.rows))
		return
	}
	// 1. Restore every device to its own rated frequency. This resets any
	// previous boost before the fresh list is applied, so recovered devices
	// simply stay at rated. A failure is logged but does not stop the boosts.
	var actErr error
	if err := c.actuator.CleanAll(); err != nil {
		actErr = err
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
			actErr = err
			c.logger.Error("nputurbo: failed to boost above-rated device(s)",
				"ids", aboveIDs, "target_mhz", maxAbove, "error", err)
		}
	}
	// 3. At-or-below-rated targets (ramp stage): pin each individually via
	// native DVFS (no side effects on other devices).
	for _, r := range belowRows {
		if err := c.actuator.BoostAtOrBelow(r.ID, r.B); err != nil {
			actErr = err
			c.logger.Error("nputurbo: failed to pin device", "id", r.ID, "target_mhz", r.B, "error", err)
		}
	}
	// Record the round only when every actuation step succeeded, so a
	// partially applied round is retried on the next tick.
	if actErr == nil {
		c.recordRound(p)
	}
	c.logger.Info(fmt.Sprintf("nputurbo: cycle done — restored all devices, boosted %d above-rated device(s), pinned %d at-or-below-rated device(s)",
		len(aboveIDs), len(belowRows)))
	c.emitMetrics(now, p, len(p.rows))
}

// recordRound remembers the plan's round as processed, enabling the
// same-round skip on subsequent fetches (no-op for rounds without the
// (id, started_at) identity — those are never deduped). It also remembers
// the plan's rows as the last applied targets for the no-op skip.
func (c *Controller) recordRound(p plan) {
	if key, ok := p.round.Key(); ok {
		c.lastKey, c.hasKey = key, true
	}
	c.lastRows = append([]BoostRow(nil), p.rows...)
	c.hasRows = true
}

// sameTargets reports whether the plan is a verified no-op: its rows are
// exactly the last fully applied rows (same devices, same target frequencies)
// and every target is already holding on the hardware (row.A ≥ row.B from the
// fresh snapshot). Set equality — not just the A ≥ B check — is load-bearing:
// a device that left the list (recovered) or changed target must run the full
// cycle, whose CleanAll is what restores recovered devices to rated; a device
// new to the list must be pinned even when already at its frequency (the
// pin's close-idle is what makes the target durable). A ≥ B rather than ==
// because the npu_turbo tool lands on 1854–1879 against a nominal 1850, so
// exact equality would never hold for boosted cards.
func (c *Controller) sameTargets(p plan) bool {
	if !c.hasRows || len(p.rows) != len(c.lastRows) {
		return false
	}
	target := make(map[int]int, len(c.lastRows))
	for _, r := range c.lastRows {
		target[r.ID] = r.B
	}
	for _, r := range p.rows {
		b, ok := target[r.ID]
		if !ok || b != r.B || r.A < r.B {
			return false
		}
	}
	return true
}

// plan is one control cycle's read-only computation (no actuation).
type plan struct {
	round   Round
	stale   bool // round older than straggler_max_age → restore-only
	cards   []SlowCard
	rows    []BoostRow
	skipped []SkipRow
}

// planBoosts runs straggler fetch + parse + per-device A/M lookup + compute
// B. It does NOT actuate. The newest round is selected from the response
// (ParseLatestRound); A is the device's live aicore_freq from the
// FreqProvider (snapshot_npu.json); M is looked up from aicore_rated_freq via
// the static rated→max table. A listed device lands in rows (with B) when
// score>1 and its frequencies are processable — including one already at or
// above its cap (B = M: the npu_turbo tool lands on 1854–1879 against the
// nominal 1850, and excluding such a card would let the cycle's restore/lower
// steps demote a still-slow card to rated); anything else is reported in
// plan.skipped with a reason (score≤1 → treated as not listed; missing freq
// data; unmapped rating). A stale round short-circuits with stale=true and no
// rows. A slow-device list with entries but an empty freq map is an error
// (caller no-ops the whole tick — same philosophy as a straggler failure: no
// input, no actuation). Shared by tick (which then actuates) and RunOnce
// (CLI preview, which does not).
func (c *Controller) planBoosts(now time.Time) (plan, error) {
	sctx, scancel := context.WithTimeout(context.Background(), c.stragglerTimeout())
	defer scancel()
	data, ferr := c.stragg.Fetch(sctx, c.cfg.StragglerURL)
	if ferr != nil {
		return plan{}, fmt.Errorf("straggler fetch: %w", ferr)
	}
	round, perr := ParseLatestRound(data)
	if perr != nil {
		return plan{}, fmt.Errorf("parse: %w", perr)
	}
	var p plan
	p.round = round
	p.cards = round.Cards
	if c.staleRound(now, round) {
		p.stale = true
		return p, nil // no rows — the tick restores-only
	}
	if len(round.Cards) == 0 {
		return p, nil
	}
	freqs := c.freqs.DeviceFreqs()
	if len(freqs) == 0 {
		return plan{}, fmt.Errorf("no NPU freq data in snapshot (snapshot_npu.json missing/empty — check snapshot.enabled and the features scope)")
	}
	for _, sc := range round.Cards {
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
		// No A>M guard here: a listed card already at or above its cap
		// (our own boost landed on 1854–1879 against the nominal 1850)
		// stays in the plan at B=M — excluding it would let the cycle's
		// restore/lower steps demote a still-slow card to rated.
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

// staleRound reports whether the round is older than StragglerMaxAge — the
// detector has stopped updating, so its last word must not be boosted on
// forever. 0 disables the check; a round without started_at (old format)
// can never be aged out.
func (c *Controller) staleRound(now time.Time, r Round) bool {
	if c.cfg.StragglerMaxAge <= 0 || r.StartedAt.IsZero() {
		return false
	}
	return now.Sub(r.StartedAt) > c.cfg.StragglerMaxAge
}

// ageString renders a round's age for logs ("unknown" without started_at).
func ageString(now, started time.Time) string {
	if started.IsZero() {
		return "unknown"
	}
	return now.Sub(started).Round(time.Second).String()
}

// formatStartedAt renders a round's started_at for logs ("unknown" when
// absent).
func formatStartedAt(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// roundAgeSeconds is the result_age_seconds metric: -1 when the round
// carries no started_at (old format), clamped at 0 against clock skew.
func roundAgeSeconds(now, started time.Time) float64 {
	if started.IsZero() {
		return -1
	}
	if d := now.Sub(started); d > 0 {
		return d.Seconds()
	}
	return 0
}

// emitMetrics builds nputurbo.* state metrics, applies the catalog filter,
// and writes them to storage for observability. boostedCount is the number
// of devices boosted this cycle (on a dedupe skip the pins still hold, so
// the plan's row count remains the truth; a stale cycle reports 0).
func (c *Controller) emitMetrics(now time.Time, p plan, boostedCount int) {
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
		{Component: "nputurbo", Name: "round_id", Value: float64(p.round.ID), Unit: "", Timestamp: now},
		{Component: "nputurbo", Name: "result_age_seconds", Value: roundAgeSeconds(now, p.round.StartedAt), Unit: "s", Timestamp: now},
	}
	filtered := metrics.Filter(ms)
	if len(filtered) > 0 {
		if err := c.store.Write(filtered); err != nil {
			c.logger.Error("nputurbo: failed to write nputurbo metrics", "error", err)
		}
	}
}
