//go:build linux

// Package nputurbo implements the NPU slow-device upclock actuator.
// It periodically HTTP GETs the external straggler detector's slow-device
// result — a detection round tagged with an id and a started_at — reads each
// device's current frequency A (npu.aicore_freq) and rated frequency
// (npu.aicore_rated_freq) from the daemon's snapshot_npu.json — keyed by
// global device id (npu_id × chips_per_card + chip_id, the same numbering
// the straggler detector reports) — maps the rating through a static table
// (1800 → 1850) to the boost cap M, and computes B = round50(min(A*score,
// M)) for every listed device with score > 1.
//
// The control loop is round-deduplicated: rounds are identified by the
// (id, started_at) pair of their doc — the id alone would collide when the
// detector process restarts and resets its counter, the timestamp does not.
// Every NEW round restores ALL devices to their own rated frequency (native
// DSMI via features/nputurbo/dvfs, the former dvfs.py semantics — no python
// or scripts on the node), then re-applies the boost set from the fresh
// list: above-rated targets in one batch (global raise via the external
// npu_turbo binary, then lower every non-target to its own rated), then
// at-or-below-rated targets pinned individually. The batch must run before
// the pins — its lower step would overwrite them. A fetch returning the
// very round that was fully applied last tick triggers no actuation at all
// (the pins already hold on the hardware), and a round older than
// straggler_max_age (0 disables) means the detector stopped updating: all
// devices are restored to rated and nothing is boosted. Devices whose
// rating is not covered by the static table, or whose current frequency is
// already above the cap, are never boosted. Docs without round metadata
// (old format) get neither dedupe nor staleness — every tick applies, the
// former stateless behavior.
//
// Linux-only: actuation needs the Ascend DSMI library (and the npu_turbo
// binary for above-rated raises). Non-Linux builds do not import this
// package; cmd/catmonitor provides a no-op nputurbo stub for other
// platforms.
package nputurbo
