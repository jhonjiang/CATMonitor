package dvfs

import "testing"

// TestReadOnlySmoke exercises the read-only operations only. It skips on
// hosts without the DSMI library (dev machines, CI). The mutating operations
// (SetAicFreq / CloseIdle / OpenIdle) must NEVER be called from tests — on
// an NPU node they would change real hardware frequency.
func TestReadOnlySmoke(t *testing.T) {
	s := Default()
	if !s.Available() {
		t.Skip("DSMI library not available on this host")
	}
	ids, err := s.DeviceIDs()
	if err != nil {
		t.Fatalf("DeviceIDs: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one device")
	}
	rated, err := s.RatedFreq(ids[0])
	if err != nil {
		t.Fatalf("RatedFreq(%d): %v", ids[0], err)
	}
	if rated <= 0 {
		t.Errorf("RatedFreq(%d) = %d, want > 0", ids[0], rated)
	}
}
