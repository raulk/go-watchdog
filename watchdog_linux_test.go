package watchdog

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/containerd/cgroups"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

// retained will hoard unreclaimable byte buffers in the heap.
var retained [][]byte

func TestCgroupsDriven_Create_Isolated(t *testing.T) {
	skipIfNotIsolated(t)

	if os.Getpid() == 1 {
		// we are running in Docker and cannot create a cgroup.
		t.Skipf("cannot create a cgroup while running in non-privileged docker")
	}

	// new cgroup limit.
	var limit = uint64(32 << 20) // 32MiB.
	createMemoryCgroup(t, limit)

	testCgroupsWatchdog(t, limit)
}

func TestCgroupsDriven_Docker_Isolated(t *testing.T) {
	skipIfNotIsolated(t)

	if os.Getpid() != 1 {
		// we are not running in a container.
		t.Skipf("test only runs inside a container")
	}

	testCgroupsWatchdog(t, uint64(DockerMemLimit))
}

func testCgroupsWatchdog(t *testing.T, limit uint64) {
	t.Cleanup(func() {
		retained = nil
	})

	runtime.GC()                  // first GC to clear any junk from other tests.
	debug.SetGCPercent(100000000) // disable GC.

	clk := clock.NewMock()
	Clock = clk

	notifyCh := make(chan struct{}, 1)
	NotifyGC = func() {
		notifyCh <- struct{}{}
	}

	err, stopFn := CgroupDriven(5*time.Second, NewAdaptivePolicy(0.5))
	require.NoError(t, err)
	defer stopFn()

	time.Sleep(200 * time.Millisecond) // give time for the watchdog to init.

	maxSlabs := limit / (1 << 20) // number of 1MiB slabs to take up the entire limit.

	// first tick; nothing should happen.
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 0) // no GC has taken place.

	// allocate 50% of limit in heap (to be added to other mem usage).
	for i := 0; i < (int(maxSlabs))/2; i++ {
		retained = append(retained, func() []byte {
			b := make([]byte, 1*1024*1024)
			for i := range b {
				b[i] = 0xff
			}
			return b
		}())
	}

	// second tick; used = just over 50%; will trigger GC.
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.NotNil(t, <-notifyCh)

	var memstats runtime.MemStats
	runtime.ReadMemStats(&memstats)
	require.EqualValues(t, 2, memstats.NumForcedGC)
}

// createMemoryCgroup creates a memory cgroup to restrict the memory available
// to this test.
func createMemoryCgroup(t *testing.T, limit uint64) {
	l := int64(limit)
	path := cgroups.NestedPath(fmt.Sprintf("/%d", time.Now().UnixNano()))
	cgroup, err := cgroups.New(cgroups.V1, path, &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit: &l,
			Swap:  &l,
		},
	})

	require.NoError(t, err, "failed to create a cgroup")
	t.Cleanup(func() {
		root, err := cgroups.Load(cgroups.V1, cgroups.RootPath)
		if err != nil {
			t.Logf("failed to resolve root cgroup: %s", err)
			return
		}
		if err = root.Add(cgroups.Process{Pid: pid}); err != nil {
			t.Logf("failed to move process to root cgroup: %s", err)
			return
		}
		if err = cgroup.Delete(); err != nil {
			t.Logf("failed to clean up temp cgroup: %s", err)
		}
	})

	log.Printf("cgroup created")

	// add process to cgroup.
	err = cgroup.Add(cgroups.Process{Pid: pid})
	require.NoError(t, err)
}
