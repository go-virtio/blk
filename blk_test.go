package blk

import "testing"

func TestDeviceType(t *testing.T) {
	if DeviceType != 2 {
		t.Errorf("DeviceType: got %d, want 2", DeviceType)
	}
}
