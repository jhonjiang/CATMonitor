//go:build linux

package nputurbo

// ComputeTargetB returns the target frequency (MHz) to boost a card whose
// current frequency is currentA and slow-card score is `score` (1.0 = normal,
// >1.0 = slower). B = round-to-nearest-step(min(currentA*score, M)).
// ok=false only when score<=1. A current frequency at or above the cap M is
// NOT a skip: the cap clamps B to M, so a listed card already at its target
// (e.g. boosted by us last round — the npu_turbo tool lands on 1854–1879
// against a nominal 1850) stays in the plan at B=M instead of being excluded
// and then demoted to rated by the next cycle's restore/lower steps.
func ComputeTargetB(currentA int, score float64, maxM, step int) (int, bool) {
	if score <= 1.0 {
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
