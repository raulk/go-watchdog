package watchdog

import (
	"os"

	"github.com/containerd/cgroups"
)

func ProcessMemoryLimit() uint64 {
	var (
		pid          = os.Getpid()
		memSubsystem = cgroups.SingleSubsystem(cgroups.V1, cgroups.Memory)
	)
	cgroup, err := cgroups.Load(memSubsystem, cgroups.PidPath(pid))
	if err != nil {
		return 0
	}
	metrics, err := cgroup.Stat()
	if err != nil {
		return 0
	}
	if metrics.Memory == nil {
		return 0
	}
	return metrics.Memory.HierarchicalMemoryLimit
}
