//go:build linux

package nputurbo

// ComputeTargetB returns the target frequency (MHz) to boost a card whose
// current frequency is currentA and slow-card score is `score` (1.0 = normal,
// >1.0 = slower). B = round-to-nearest-step(min(currentA*score, M)).
// ok=false when no boost is warranted: score<=1, or the rounded target
// does not exceed currentA (gain wiped by step quantization).
func ComputeTargetB(currentA int, score float64, maxM, step int) (int, bool) {
	if score <= 1.0 {
		return 0, false
	}
	raw := float64(currentA) * score
	b := roundStep(int(raw+0.5), step) // round half up to nearest step
	if b > maxM {
		b = maxM
	}
	if b <= currentA {
		return 0, false
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
