//go:build linux

// Package nputurbo implements the NPU slow-device upclock actuator.
// It periodically HTTP GETs the external straggler detector's slow-device
// result, reads each device's current frequency A (npu.aicore_freq) and rated
// frequency (npu.aicore_rated_freq) from the daemon's snapshot_npu.json —
// keyed by global device id (npu_id × chips_per_card + chip_id, the same
// numbering the straggler detector reports) — maps the rating through a
// static table (1800 → 1850) to the boost cap M, and computes
// B = round50(min(A*score, M)) for every listed device with score > 1.
//
// The control loop is stateless: every cycle restores ALL devices to their
// own rated frequency (native DSMI via features/nputurbo/dvfs, the former
// dvfs.py semantics — no python or scripts on the node), then re-applies the
// boost set from the fresh list: above-rated targets in one batch (global
// raise via the external npu_turbo binary, then lower every non-target to
// its own rated), then at-or-below-rated targets pinned individually. The
// batch must run before the pins — its lower step would overwrite them.
// Devices whose rating is not covered by the static table, or whose current
// frequency is already above the cap, are never boosted.
//
// Linux-only: actuation needs the Ascend DSMI library (and the npu_turbo
// binary for above-rated raises). Non-Linux builds do not import this
// package; cmd/catmonitor provides a no-op nputurbo stub for other
// platforms.
package nputurbo
