package watchdog

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/containerd/cgroups"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
)

// TestOnMemoryPressure tests that the watchdog responds to kernel memory
// pressure notifications. This test will not run in a standard (non-priviliged)
// container, because container engines like Docker mount the cgroup read-only,
// which prevents us from writing to the memory pressure control fd to request
// receiving notifications.
func TestOnMemoryPressure(t *testing.T) {
	fmt.Println("running")
	if os.Getpid() == 1 {
		t.Skip("test cannot run in a container, due to unwriteable /sys/fs/cgroups mount")
	}

	var limit = uint64(32 << 20) // 32MiB.
	createMemoryCgroup(t, limit)

	err, _ := OnMemoryPressure()
	require.NoError(t, err)
	// defer stopFn()

	var memstats runtime.MemStats
	var retained [][]byte
	slabs := int(float64(limit)*0.60) / (1 << 20)
	fmt.Println(slabs)
	for i := 0; i < slabs; i++ {
		retained = append(retained, make([]byte, 1*1024*1024))
		runtime.ReadMemStats(&memstats)
		fmt.Println(memstats.NumForcedGC)
	}

	for _, b := range retained {
		for j := range b {
			b[j]++
		}
	}

	time.Sleep(1 * time.Hour)

}

func discoverMemoryLimit(t *testing.T) uint64 {
	ns := cgroups.SingleSubsystem(cgroups.V1, cgroups.Memory)
	cgroup, err := cgroups.Load(ns, cgroups.RootPath)
	require.NoError(t, err)

	stat, err := cgroup.Stat()
	require.NoError(t, err)

	return stat.Memory.Usage.Limit
}

// createMemoryCgroup creates a memory cgroup to restrict the memory available
// to this test.
func createMemoryCgroup(t *testing.T, limit uint64) {
	l := int64(limit)
	path := cgroups.StaticPath(fmt.Sprintf("/%d", time.Now().UnixNano()))
	cgroup, err := cgroups.New(cgroups.V1, path, &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit: &l,
			Swap:  &l,
		},
	})
	require.NoError(t, err, "failed to create a cgroup")
	t.Cleanup(func() {
		_ = cgroup.Delete()
	})

	log.Printf("cgroup created")

	// add process to cgroup.
	err = cgroup.Add(cgroups.Process{Pid: pid})
	require.NoError(t, err)
}
