//go:build linux

package nputurbo

// ComputeTargetB returns the target frequency (MHz) to boost a card whose
// current frequency is currentA and slow-card score is `score` (1.0 = normal,
// >1.0 = slower). B = round-to-nearest-step(min(currentA*score, M)).
// ok=false when no boost is warranted: score<=1, or the current frequency is
// already above the cap M (we never downclock — such a card is left alone).
// Note B<=A does NOT skip: a card already at its target (e.g. boosted by us
// last tick, now reflected in the snapshot's aicore_freq) stays in the plan so
// it is not misread as recovered.
func ComputeTargetB(currentA int, score float64, maxM, step int) (int, bool) {
	if score <= 1.0 {
		return 0, false
	}
	if currentA > maxM {
		return 0, false
	}
	raw := float64(currentA) * score
	b := roundStep(int(raw+0.5), step) // round half up to nearest step
	if b > maxM {
		b = maxM
	}
	return b, true
}

// roundStep rounds v to the nearest multiple of step (ties go up).
func roundStep(v, step int) int {
	if step <= 0 {
		return v
	}
	return ((v + step/2) / step) * step
}
