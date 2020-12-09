// +build !linux

package watchdog

func ProcessMemoryLimit() uint64 {
	return 0
}
