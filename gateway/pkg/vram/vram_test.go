package vram

import (
	"testing"
)

func TestQuery(t *testing.T) {
	stats, err := Query(0)
	if err != nil {
		t.Skipf("NVML not available: %v", err)
	}

	if stats.TotalMB == 0 {
		t.Fatal("TotalMB should be > 0")
	}

	sum := stats.FreeMB + stats.UsedMB
	if sum < stats.TotalMB-10 || sum > stats.TotalMB+10 {
		t.Errorf("FreeMB(%d) + UsedMB(%d) = %d, expected ~TotalMB(%d)", stats.FreeMB, stats.UsedMB, sum, stats.TotalMB)
	}

	t.Logf("GPU %d: Total=%dMB Used=%dMB Free=%dMB", stats.DeviceIndex, stats.TotalMB, stats.UsedMB, stats.FreeMB)
}
