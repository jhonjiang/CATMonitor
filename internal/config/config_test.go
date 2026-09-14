package config

import (
	"testing"
	"time"
)

func TestDefaultNputurboConfig(t *testing.T) {
	cfg := Default()
	if cfg.Nputurbo.Enabled {
		t.Error("Nputurbo.Enabled should default false (opt-in)")
	}
	if !cfg.Nputurbo.DryRun {
		t.Error("Nputurbo.DryRun should default true (judge+log only)")
	}
	if cfg.Nputurbo.StepMhz != 50 {
		t.Errorf("Nputurbo.StepMhz = %d, want 50", cfg.Nputurbo.StepMhz)
	}
	if !cfg.Nputurbo.RestoreOnShutdown {
		t.Error("Nputurbo.RestoreOnShutdown should default true")
	}
	if cfg.Nputurbo.NpuTurboBin != "/home/jw/npu_turbo" {
		t.Errorf("Nputurbo.NpuTurboBin = %q, want /home/jw/npu_turbo", cfg.Nputurbo.NpuTurboBin)
	}
	if cfg.Nputurbo.StragglerURL != "" {
		t.Errorf("Nputurbo.StragglerURL = %q, want empty default", cfg.Nputurbo.StragglerURL)
	}
	if cfg.Nputurbo.StragglerTimeout != 10*time.Second {
		t.Errorf("Nputurbo.StragglerTimeout = %v, want 10s", cfg.Nputurbo.StragglerTimeout)
	}
}
