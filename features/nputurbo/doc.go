//go:build linux

// Package nputurbo implements the NPU slow-device upclock actuator.
// It periodically HTTP GETs the external straggler detector's slow-device
// result, reads each device's current frequency A (npu.aicore_freq) and rated
// frequency (npu.aicore_rated_freq) from the daemon's snapshot_npu.json —
// keyed by global device id (npu_id × chips_per_card + chip_id, the same
// numbering the straggler detector reports) — maps the rating through a
// static table (1800 → 1850) to the boost cap M, computes
// B = round50(min(A*score, M)) for every listed device with score > 1 (score
// <=1 in the list counts as not listed), and execs npu_turbo_one.sh inject to
// raise the device's frequency. A previously boosted device that leaves the
// list (or shows score<=1) triggers a clean (restore all devices to baseline)
// + re-inject of the remaining slow devices. Ratings not covered by the
// static table, and devices already above the cap, are never touched.
//
// Linux-only: actuation execs an external binary expected on Ascend hosts.
// Non-Linux builds do not import this package; cmd/catmonitor provides a
// no-op nputurbo stub for other platforms.
package nputurbo
