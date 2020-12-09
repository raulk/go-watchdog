package watchdog

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

// These integration tests are a hugely non-deterministic, but necessary to get
// good coverage and confidence. The Go runtime makes its own pacing decisions,
// and those may vary based on machine, OS, kernel memory management, other
// running programs, exogenous memory pressure, and Go runtime versions.
//
// The assertions we use here are lax, but should be sufficient to serve as a
// reasonable litmus test of whether the watchdog is doing what it's supposed
// to or not.

var (
	limit uint64 = 64 << 20 // 64MiB.
)

func init() {
	Logger = &stdlog{log: log.New(os.Stdout, "[watchdog test] ", log.LstdFlags|log.Lmsgprefix), debug: true}
}

func TestControl(t *testing.T) {
	debug.SetGCPercent(100)

	// retain 1MiB every iteration, up to 100MiB (beyond heap limit!).
	var retained [][]byte
	for i := 0; i < 100; i++ {
		b := make([]byte, 1*1024*1024)
		for i := range b {
			b[i] = byte(i)
		}
		retained = append(retained, b)
	}

	for _, b := range retained {
		for i := range b {
			b[i] = byte(i)
		}
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	require.LessOrEqual(t, ms.NumGC, uint32(5)) // a maximum of 8 GCs should've happened.
	require.Zero(t, ms.NumForcedGC)             // no forced GCs.
}

func TestHeapDriven(t *testing.T) {
	// we can't mock ReadMemStats, because we're relying on the go runtime to
	// enforce the GC run, and the go runtime won't use our mock. Therefore, we
	// need to do the actual thing.
	debug.SetGCPercent(100)

	clk := clock.NewMock()
	Clock = clk

	observations := make([]*runtime.MemStats, 0, 100)
	NotifyFired = func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		observations = append(observations, &ms)
	}

	// limit is 64MiB.
	err, stopFn := HeapDriven(limit, NewAdaptivePolicy(0.5))
	require.NoError(t, err)
	defer stopFn()

	time.Sleep(500 * time.Millisecond) // give time for the watchdog to init.

	// retain 1MiB every iteration, up to 100MiB (beyond heap limit!).
	var retained [][]byte
	for i := 0; i < 100; i++ {
		retained = append(retained, make([]byte, 1*1024*1024))
	}

	for _, o := range observations {
		fmt.Println("heap alloc:", o.HeapAlloc, "next gc:", o.NextGC, "gc count:", o.NumGC, "forced gc:", o.NumForcedGC)
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	require.GreaterOrEqual(t, ms.NumGC, uint32(9)) // over 9 GCs should've taken place.
}

func TestSystemDriven(t *testing.T) {
	debug.SetGCPercent(100)

	clk := clock.NewMock()
	Clock = clk

	// mock the system reporting.
	var actualUsed uint64
	sysmemFn = func(g *gosigar.Mem) error {
		g.ActualUsed = actualUsed
		return nil
	}

	// limit is 64MiB.
	err, stopFn := SystemDriven(limit, 5*time.Second, NewAdaptivePolicy(0.5))
	require.NoError(t, err)
	defer stopFn()

	time.Sleep(200 * time.Millisecond) // give time for the watchdog to init.

	notifyCh := make(chan struct{}, 1)
	NotifyFired = func() {
		notifyCh <- struct{}{}
	}

	// first tick; used = 0.
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 0) // no GC has taken place.

	// second tick; used = just over 50%; will trigger GC.
	actualUsed = (limit / 2) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 1)
	<-notifyCh

	// third tick; just below 75%; no GC.
	actualUsed = uint64(float64(limit)*0.75) - 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 0)

	// fourth tick; 75% exactly; will trigger GC.
	actualUsed = uint64(float64(limit)*0.75) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 1)
	<-notifyCh

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	require.GreaterOrEqual(t, ms.NumForcedGC, uint32(2))
}
