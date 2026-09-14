//go:build linux

package nputurbo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/snapshot"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

func TestMaxBoostForRated(t *testing.T) {
	if m, ok := MaxBoostForRated(1800); !ok || m != 1850 {
		t.Errorf("MaxBoostForRated(1800) = (%d,%v), want (1850,true)", m, ok)
	}
	if _, ok := MaxBoostForRated(2000); ok {
		t.Error("MaxBoostForRated(2000) should be unmapped")
	}
	if _, ok := MaxBoostForRated(0); ok {
		t.Error("MaxBoostForRated(0) should be unmapped")
	}
}

// writeNpuSnapshot writes a CompSnapshot fixture with the given metrics to
// dir/snapshot_npu.json and returns the provider over that dir.
func writeNpuSnapshot(t *testing.T, dir string, metrics []collector.Metric) SnapshotFreqProvider {
	t.Helper()
	cs := snapshot.CompSnapshot{Component: "npu", Timestamp: time.Now(), Metrics: metrics}
	data, err := json.Marshal(&cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot_npu.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return SnapshotFreqProvider{Dir: dir}
}

func npuFreqMetric(name, npuID, chipID string, v float64) collector.Metric {
	return collector.Metric{
		Component: "npu", Name: name, Value: v, Unit: "MHz",
		Labels:    map[string]string{"npu_id": npuID, "chip_id": chipID},
		Timestamp: time.Now(),
	}
}

func TestDeviceFreqsSingleChip(t *testing.T) {
	// Single-chip nodes: chips_per_card=1 → device id == npu_id.
	dir := t.TempDir()
	p := writeNpuSnapshot(t, dir, []collector.Metric{
		npuFreqMetric("aicore_freq", "0", "0", 1800),
		npuFreqMetric("aicore_rated_freq", "0", "0", 1800),
		npuFreqMetric("aicore_freq", "1", "0", 1750),
		npuFreqMetric("aicore_rated_freq", "1", "0", 1800),
	})
	got := p.DeviceFreqs()
	if len(got) != 2 {
		t.Fatalf("expected 2 devices, got %d (%v)", len(got), got)
	}
	if got[0] != (DeviceFreq{Current: 1800, Rated: 1800}) {
		t.Errorf("device 0 = %+v, want {1800 1800}", got[0])
	}
	if got[1] != (DeviceFreq{Current: 1750, Rated: 1800}) {
		t.Errorf("device 1 = %+v, want {1750 1800}", got[1])
	}
}

func TestDeviceFreqsDualChipGlobalIDs(t *testing.T) {
	// A3 dual-chip: chips_per_card=2 → device id = npu_id×2 + chip_id.
	// npu 2 chip 0/1 → devices 4/5; each chip is an independent entry (no
	// per-card min aggregation).
	dir := t.TempDir()
	p := writeNpuSnapshot(t, dir, []collector.Metric{
		npuFreqMetric("aicore_freq", "2", "0", 1800),
		npuFreqMetric("aicore_rated_freq", "2", "0", 1800),
		npuFreqMetric("aicore_freq", "2", "1", 1750),
		npuFreqMetric("aicore_rated_freq", "2", "1", 1800),
	})
	got := p.DeviceFreqs()
	if len(got) != 2 {
		t.Fatalf("expected 2 devices, got %d (%v)", len(got), got)
	}
	if got[4] != (DeviceFreq{Current: 1800, Rated: 1800}) {
		t.Errorf("device 4 (npu2/chip0) = %+v, want {1800 1800}", got[4])
	}
	if got[5] != (DeviceFreq{Current: 1750, Rated: 1800}) {
		t.Errorf("device 5 (npu2/chip1) = %+v, want {1750 1800} (per-chip, not min)", got[5])
	}
}

func TestDeviceFreqsMissingFields(t *testing.T) {
	dir := t.TempDir()
	// device 0: current but no rated; device 1: rated but no current;
	// device 2's metric is not a freq (ignored).
	p := writeNpuSnapshot(t, dir, []collector.Metric{
		npuFreqMetric("aicore_freq", "0", "0", 1800),
		npuFreqMetric("aicore_rated_freq", "1", "0", 1800),
		npuFreqMetric("temperature", "2", "0", 42),
	})
	got := p.DeviceFreqs()
	if got[0] != (DeviceFreq{Current: 1800, Rated: 0}) {
		t.Errorf("device 0 = %+v, want current-only entry", got[0])
	}
	if got[1] != (DeviceFreq{Current: 0, Rated: 1800}) {
		t.Errorf("device 1 = %+v, want rated-only entry", got[1])
	}
	if _, ok := got[2]; ok {
		t.Error("non-freq metrics must not create entries")
	}
}

func TestDeviceFreqsNoChipIDFallsBackToNpuID(t *testing.T) {
	// Metrics without a chip_id label (card-level, e.g. hccn_tool's) fall
	// back to the bare npu_id. aicore_freq always carries chip_id, so this is
	// defensive only.
	dir := t.TempDir()
	p := writeNpuSnapshot(t, dir, []collector.Metric{
		{
			Component: "npu", Name: "aicore_freq", Value: 1800, Unit: "MHz",
			Labels:    map[string]string{"npu_id": "3"},
			Timestamp: time.Now(),
		},
	})
	got := p.DeviceFreqs()
	if got[3] != (DeviceFreq{Current: 1800, Rated: 0}) {
		t.Errorf("device 3 = %+v, want fallback to bare npu_id", got[3])
	}
}

func TestDeviceFreqsMissingFile(t *testing.T) {
	p := SnapshotFreqProvider{Dir: t.TempDir()}
	if got := p.DeviceFreqs(); len(got) != 0 {
		t.Errorf("missing snapshot_npu.json should yield empty map, got %v", got)
	}
}
