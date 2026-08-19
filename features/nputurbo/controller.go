//go:build linux

package nputurbo

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/snapshot"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/metrics"
)

// Config holds the controller tunables (mapped from config.NputurboConfig by
// the caller in cmd/catmonitor).
type Config struct {
	Interval          time.Duration
	StragglerCmd      string
	ResultPath        string
	StragglerTimeout  time.Duration
	NpuTurboCmd       string
	NpuTurboTimeout   time.Duration
	MaxFreqMhz        int
	StepMhz           int
	DryRun            bool
	RestoreOnShutdown bool
	SnapshotDir       string
	Logger            *slog.Logger
}

// Storage is the subset of the storage interface the controller needs (write
// state metrics for observability).
type Storage interface {
	Write(ms []collector.Metric) error
}

// StragglerSource is the seam for the straggler exec source (real =
// straggler.Default(); tests inject a fake).
type StragglerSource interface {
	Available() bool
	Run(ctx context.Context, cmdTemplate, resultPath string) error
}

// Controller is the nputurbo control loop. Each tick it runs the straggler
// detector to refresh the slow-card result file, parses it, reads current
// AICore frequencies from snapshot_npu.json, reconciles the boosted set
// (restore disappeared, boost listed), and emits state metrics.
type Controller struct {
	cfg      Config
	stragg   StragglerSource
	actuator *Actuator
	store    Storage
	logger   *slog.Logger
}

func NewController(cfg Config, stragg StragglerSource, actuator *Actuator, store Storage) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Controller{cfg: cfg, stragg: stragg, actuator: actuator, store: store, logger: cfg.Logger}
}

// Run is the control loop. Ticks at cfg.Interval until ctx is cancelled.
// Shutdown (main.go) cancels ctx then calls Restore() for best-effort
// frequency recovery.
func (c *Controller) Run(ctx context.Context) {
	if c.cfg.Interval <= 0 {
		c.cfg.Interval = 60 * time.Second
	}
	c.logger.Info("nputurbo controller started",
		"interval", c.cfg.Interval, "dry_run", c.cfg.DryRun,
		"straggler_cmd", c.cfg.StragglerCmd, "result_path", c.cfg.ResultPath)
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
	if c.cfg.SnapshotDir == "" {
		c.logger.Warn("nputurbo: snapshot dir not set; cannot read aicore_freq; no-op")
		return
	}
	// planBoosts runs straggler + parse + read snapshot freqs + compute B
	// per card (no actuation). On straggler/parse failure it returns err
	// → complete no-op: we have no fresh list to reconcile against, so the
	// previously-boosted state is left untouched (not restored).
	_, rows, _, err := c.planBoosts()
	if err != nil {
		c.logger.Error("nputurbo: plan failed; no-op this cycle", "error", err)
		return
	}
	// Reconcile to desired state. desired = {id: B} for cards that warrant a boost.
	desired := make(map[int]int, len(rows))
	for _, r := range rows {
		if r.WouldBoost {
			desired[r.ID] = r.B
		}
	}
	c.logger.Info("nputurbo: straggler tick",
		"slow_cards", len(rows), "would_boost", len(desired), "result_path", c.cfg.ResultPath)
	if c.cfg.DryRun {
		for _, r := range rows {
			if r.WouldBoost {
				c.logger.Info("nputurbo dry-run: would boost", "id", r.ID, "A", r.A, "score", r.Score, "B", r.B)
			}
		}
		c.emitMetrics(now)
		return
	}
	current := c.actuator.LastAppliedMap()
	// A boosted card no longer in the desired list has recovered. Since clean
	// is all-or-nothing, any recovery forces clean + full re-inject (still-slow
	// cards are re-boosted after clean). When no card recovered, only inject
	// new/changed cards (idempotent re-set; no flicker on stable cards).
	recoveredCount := 0
	for id := range current {
		if _, ok := desired[id]; !ok {
			recoveredCount++
		}
	}
	if recoveredCount > 0 && len(current) > 0 {
		c.logger.Info("nputurbo: cards recovered → clean + re-inject",
			"recovered", recoveredCount, "previously_boosted", len(current), "re_injected", len(desired))
		rctx, rcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
		_ = c.actuator.RestoreAll(rctx) // actuator logs clean success/failure incl. output
		rcancel()
		for id, b := range desired {
			bctx, bcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
			_ = c.actuator.Boost(bctx, id, b) // actuator logs inject incl. output
			bcancel()
		}
	} else {
		injected := 0
		for id, b := range desired {
			if c.actuator.LastApplied(id) == b {
				continue // idempotent: already at target
			}
			bctx, bcancel := context.WithTimeout(context.Background(), c.npuTurboTimeout())
			if err := c.actuator.Boost(bctx, id, b); err == nil {
				injected++
			} // actuator logs inject success/failure incl. output
			bcancel()
		}
		switch {
		case injected > 0:
			c.logger.Info("nputurbo: injected new/changed cards", "count", injected, "skipped_idempotent", len(desired)-injected)
		case len(desired) > 0:
			c.logger.Info("nputurbo: all boosted cards stable (idempotent, no change)", "desired", len(desired))
		}
	}
	c.emitMetrics(now)
}

// planBoosts runs straggler + parse + read snapshot freqs + compute B per
// card. It does NOT actuate. Returns the parsed cards, per-card plan rows,
// and the freqs map. On straggler/parse failure returns err (caller no-ops).
// Shared by tick (which then actuates) and RunOnce (CLI preview, which does not).
func (c *Controller) planBoosts() (cards []SlowCard, rows []BoostRow, freqs map[int]int, err error) {
	sctx, scancel := context.WithTimeout(context.Background(), c.stragglerTimeout())
	defer scancel()
	if err := c.stragg.Run(sctx, c.cfg.StragglerCmd, c.cfg.ResultPath); err != nil {
		return nil, nil, nil, fmt.Errorf("straggler run: %w", err)
	}
	data, rerr := os.ReadFile(c.cfg.ResultPath)
	if rerr != nil {
		return nil, nil, nil, fmt.Errorf("read result %s: %w", c.cfg.ResultPath, rerr)
	}
	cards, perr := ParseSlowCards(data)
	if perr != nil {
		return nil, nil, nil, fmt.Errorf("parse: %w", perr)
	}
	freqs = readAicoreFreqs(c.cfg.SnapshotDir)
	for _, sc := range cards {
		a, ok := freqs[sc.ID]
		if !ok {
			c.logger.Warn("nputurbo: no aicore_freq in snapshot for card; skip", "id", sc.ID)
			continue
		}
		b, okB := ComputeTargetB(a, sc.Score, c.cfg.MaxFreqMhz, c.cfg.StepMhz)
		rows = append(rows, BoostRow{ID: sc.ID, A: a, B: b, Score: sc.Score, WouldBoost: okB})
	}
	return cards, rows, freqs, nil
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
	return 10 * time.Second
}

// readAicoreFreqs reads snapshot_npu.json and returns map[npu_id]aicore_freq.
func readAicoreFreqs(snapshotDir string) map[int]int {
	out := map[int]int{}
	cs, err := snapshot.ReadComp(filepath.Join(snapshotDir, "snapshot_npu.json"))
	if err != nil {
		return out
	}
	for _, m := range cs.Metrics {
		if m.Component == "npu" && m.Name == "aicore_freq" && m.Labels != nil {
			if id, err := strconv.Atoi(m.Labels["npu_id"]); err == nil {
				out[id] = int(m.Value)
			}
		}
	}
	return out
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
			c.logger.Error("nputurbo: metric write failed", "error", err)
		}
	}
}
