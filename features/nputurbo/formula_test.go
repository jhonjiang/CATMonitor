//go:build linux

package nputurbo

import "testing"

func TestComputeTargetB(t *testing.T) {
	cases := []struct {
		name    string
		A       int
		score   float64
		M, step int
		wantB   int
		wantOK  bool
	}{
		{"basic", 1700, 1.1, 1900, 50, 1850, true},    // 1700*1.1=1870 → round50=1850 (no cap)
		{"capAtMax", 1800, 1.2, 1900, 50, 1900, true}, // 2160 → round50=2150 → cap 1900
		{"scoreLE1skip", 1800, 1.0, 1900, 50, 0, false},
		{"roundNoGain", 1800, 1.001, 1900, 50, 0, false},  // 1801.8 → round50=1800 == A → skip
		{"exactStep", 1800, 1.0277, 1900, 50, 1850, true}, // 1849.86 → round50=1850
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotB, gotOK := ComputeTargetB(c.A, c.score, c.M, c.step)
			if gotB != c.wantB || gotOK != c.wantOK {
				t.Errorf("ComputeTargetB(%d,%.4f,%d,%d) = (%d,%v), want (%d,%v)",
					c.A, c.score, c.M, c.step, gotB, gotOK, c.wantB, c.wantOK)
			}
		})
	}
}
