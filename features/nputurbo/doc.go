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
// A fetch returning the very round that was fully applied last tick triggers
// no actuation at all (the pins already hold on the hardware), and neither
// does a new round whose plan matches the last applied one with every target
// verifiably still holding (A ≥ B from the fresh snapshot) — the steady state
// for a card already boosted to its cap re-runs nothing, where the former
// model would dip it to rated for the ~18s npu_turbo exec every round. Any
// other new round restores ALL devices to their own rated frequency (native
// DSMI via features/nputurbo/dvfs, the former dvfs.py semantics — no python
// or scripts on the node), then re-applies the boost set from the fresh
// list: above-rated targets in one batch (global raise via the external
// npu_turbo binary, then lower every non-target to its own rated), then
// at-or-below-rated targets pinned individually. The batch must run before
// the pins — its lower step would overwrite them. A round older than
// straggler_max_age (0s disables) means the detector stopped updating: all
// devices are restored to rated and nothing is boosted. Devices whose
// rating is not covered by the static table are never boosted; a listed
// device already at or above its cap is managed to the cap (B = M) rather
// than skipped — the npu_turbo tool lands on 1854–1879 against the nominal
// 1850, so skipping such a card would let the next cycle's restore demote a
// still-slow card to rated. Docs without round metadata (old format) get
// neither dedupe nor staleness — every tick applies, the former stateless
// behavior.
//
// Linux-only: actuation needs the Ascend DSMI library (and the npu_turbo
// binary for above-rated raises). Non-Linux builds do not import this
// package; cmd/catmonitor provides a no-op nputurbo stub for other
// platforms.
package nputurbo
