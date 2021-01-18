// +build !linux

package watchdog

import "testing"

func TestCgroupsDriven_Create_Isolated(t *testing.T) {
	// this test only runs on linux.
	t.Skip("test only valid on linux")
}

func TestCgroupsDriven_Docker_Isolated(t *testing.T) {
	// this test only runs on linux.
	t.Skip("test only valid on linux")
}
