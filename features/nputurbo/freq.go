//go:build linux

package nputurbo

import (
	"path/filepath"
	"strconv"

	"github.com/Computing-Availability-Tools/CATMonitor/features/snapshot"
)

// ratedMaxBoost maps a device's rated (maximum) AICore frequency, as reported
// by the npu collector's aicore_rated_freq metric, to the maximum boost
// target allowed for that rating. M in the boost formula comes exclusively
// from this table: a device whose rated frequency is not listed here is never
// boosted (unknown hardware → no actuation, no fallback cap).
var ratedMaxBoost = map[int]int{
	1800: 1850,
}

// MaxBoostForRated returns the boost cap for a rated frequency and whether the
// rating is covered by the static table.
func MaxBoostForRated(rated int) (int, bool) {
	m, ok := ratedMaxBoost[rated]
	return m, ok
}

// DeviceFreq holds the per-device frequencies extracted from
// snapshot_npu.json: Current is aicore_freq (the boost-formula baseline A),
// Rated is aicore_rated_freq (the key into ratedMaxBoost). A zero field means
// "missing" — the caller skips such devices.
type DeviceFreq struct {
	Current int // MHz (aicore_freq)
	Rated   int // MHz (aicore_rated_freq)
}

// FreqProvider supplies per-device (global device id) current + rated AICore
// frequencies. The controller consults it every tick; the CLI preview uses it
// the same way. Real implementation: SnapshotFreqProvider; tests inject a
// fake. The device id matches the straggler slow-card id: npu_id ×
// chips_per_card + chip_id — the same global device numbering stragglerout
// writes its KPI files with, so the id fetched from straggler_url aligns
// directly.
type FreqProvider interface {
	DeviceFreqs() map[int]DeviceFreq
}

// SnapshotFreqProvider reads snapshot_npu.json (written by the daemon, the
// sole snapshot producer) and extracts npu.aicore_freq / npu.aicore_rated_freq
// keyed by global device id = npu_id × chips_per_card + chip_id, where
// chips_per_card is derived as max(chip_id)+1 from the snapshot (2 on A3
// dual-chip, 1 otherwise) — mirroring stragglerout's device numbering, which
// the straggler detector reports back. On single-chip nodes the id equals the
// bare npu_id. Metrics without a chip_id label fall back to the bare npu_id
// (aicore_freq / aicore_rated_freq are per-device DCMI metrics and always
// carry chip_id, so the fallback is defensive only). A missing/unreadable
// file yields an empty map (the controller treats that as "no input" and
// no-ops the tick).
type SnapshotFreqProvider struct {
	Dir string // snapshot dir containing snapshot_npu.json
}

// deviceKey identifies one metric's source chip as seen in the snapshot
// labels. hasChip distinguishes an absent chip_id label (card-level metric)
// from chip_id=0 (first chip of a dual-chip card).
type deviceKey struct {
	npuID   int
	chipID  int
	hasChip bool
}

func (p SnapshotFreqProvider) DeviceFreqs() map[int]DeviceFreq {
	out := map[int]DeviceFreq{}
	cs, err := snapshot.ReadComp(filepath.Join(p.Dir, "snapshot_npu.json"))
	if err != nil {
		return out
	}
	cur := map[deviceKey]int{}
	rated := map[deviceKey]int{}
	chips := 1
	for _, m := range cs.Metrics {
		if m.Component != "npu" || m.Labels == nil {
			continue
		}
		npuID, err := strconv.Atoi(m.Labels["npu_id"])
		if err != nil {
			continue
		}
		k := deviceKey{npuID: npuID}
		if chip, err := strconv.Atoi(m.Labels["chip_id"]); err == nil && chip >= 0 {
			k.chipID = chip
			k.hasChip = true
			if chip+1 > chips {
				chips = chip + 1
			}
		}
		switch m.Name {
		case "aicore_freq":
			cur[k] = int(m.Value)
		case "aicore_rated_freq":
			rated[k] = int(m.Value)
		}
	}
	globalID := func(k deviceKey) int {
		if k.hasChip {
			return k.npuID*chips + k.chipID
		}
		return k.npuID
	}
	curIDs := make(map[int]int, len(cur))
	for k, v := range cur {
		curIDs[globalID(k)] = v
	}
	ratedIDs := make(map[int]int, len(rated))
	for k, v := range rated {
		ratedIDs[globalID(k)] = v
	}
	for id, v := range curIDs {
		out[id] = DeviceFreq{Current: v, Rated: ratedIDs[id]}
	}
	// Devices with rated but no current freq still get an entry (Current=0);
	// the caller treats any zero field as missing and skips the device.
	for id, r := range ratedIDs {
		if _, ok := out[id]; !ok {
			out[id] = DeviceFreq{Rated: r}
		}
	}
	return out
}

// noopFreqProvider is the nil-safe default: reports no frequency data.
type noopFreqProvider struct{}

func (noopFreqProvider) DeviceFreqs() map[int]DeviceFreq { return map[int]DeviceFreq{} }
