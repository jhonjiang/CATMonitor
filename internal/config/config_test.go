package config

import "testing"

func TestDefaultNputurboConfig(t *testing.T) {
	cfg := Default()
	if cfg.Nputurbo.Enabled {
		t.Error("Nputurbo.Enabled should default false (opt-in)")
	}
	if !cfg.Nputurbo.DryRun {
		t.Error("Nputurbo.DryRun should default true (judge+log only)")
	}
	if cfg.Nputurbo.MaxFreqMhz != 1900 {
		t.Errorf("Nputurbo.MaxFreqMhz = %d, want 1900", cfg.Nputurbo.MaxFreqMhz)
	}
	if cfg.Nputurbo.StepMhz != 50 {
		t.Errorf("Nputurbo.StepMhz = %d, want 50", cfg.Nputurbo.StepMhz)
	}
	if !cfg.Nputurbo.RestoreOnShutdown {
		t.Error("Nputurbo.RestoreOnShutdown should default true")
	}
	if cfg.Nputurbo.NpuTurboCmd != "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}" {
		t.Errorf("Nputurbo.NpuTurboCmd = %q", cfg.Nputurbo.NpuTurboCmd)
	}
	if cfg.Nputurbo.NpuTurboCleanCmd != "/home/jw/npu_turbo_one.sh clean" {
		t.Errorf("Nputurbo.NpuTurboCleanCmd = %q", cfg.Nputurbo.NpuTurboCleanCmd)
	}
}
