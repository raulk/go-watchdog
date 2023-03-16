package watchdog

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/opencontainers/runtime-spec/specs-go"
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
func createMemoryCgroup(t *testing.T, limit uint64) {
	switch cgroups.Mode() {
	case cgroups.Unified:
		createMemoryCgroup2(t, limit)
	case cgroups.Legacy:
		createMemoryCgroup1(t, limit)
	case cgroups.Unavailable:
		fallthrough
	default:
		t.Logf("Cgroups not supported in this environment")
	}
}

// createMemoryCgroup creates a memory cgroup to restrict the memory available
// to this test.
func createMemoryCgroup1(t *testing.T, limit uint64) {
	l := int64(limit)
	path := cgroup1.NestedPath(fmt.Sprintf("/%d", time.Now().UnixNano()))
	cgroup, err := cgroup1.New(path, &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit: &l,
			Swap:  &l,
		},
	})

	require.NoError(t, err, "failed to create a cgroup1")
	t.Cleanup(func() {
		root, err := cgroup1.Load(cgroup1.RootPath)
		if err != nil {
			t.Logf("failed to resolve root cgroup: %s", err)
			return
		}
		if err = root.Add(cgroup1.Process{Pid: pid}); err != nil {
			t.Logf("failed to move process to root cgroup: %s", err)
			return
		}
		if err = cgroup.Delete(); err != nil {
			t.Logf("failed to clean up temp cgroup: %s", err)
		}
	})

	log.Printf("cgroup1 created")

	// add process to cgroup.
	err = cgroup.Add(cgroup1.Process{Pid: pid})
	require.NoError(t, err)
}
func createMemoryCgroup2(t *testing.T, limit uint64) {
	l := int64(limit)
	path := fmt.Sprintf("/%d", time.Now().UnixNano())
	cgroup, err := cgroup2.NewManager("/sys/fs/cgroup", path, cgroup2.ToResources(&specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit: &l,
			Swap:  &l,
		},
	}))
	require.NoError(t, err, "failed to create a cgroup2")
	t.Cleanup(func() {
		root, err := cgroup2.Load("/")
		if err != nil {
			t.Logf("failed to resolve root cgroup2: %s", err)
			return
		}
		if err = cgroup.MoveTo(root); err != nil {
			t.Logf("failed to move processes to root cgroup2: %s", err)
			return
		}
		if err = cgroup.Delete(); err != nil {
			t.Logf("failed to clean up temp cgroup2: %s", err)
		}
	})
	pids, _ := cgroup.Procs(false)
	log.Printf("cgroup2 %d created, adding pid %d", pids, pid)
	// add process to cgroup.
	err = cgroup.AddProc(uint64(pid))
	require.NoError(t, err)
}
