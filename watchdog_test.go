package watchdog

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

const (
	// EnvTestIsolated is a marker property for the runner to confirm that this
	// test is running in isolation (i.e. a dedicated process).
	EnvTestIsolated = "TEST_ISOLATED"

	// EnvTestDockerMemLimit is the memory limit applied in a docker container.
	EnvTestDockerMemLimit = "TEST_DOCKER_MEMLIMIT"
)

// DockerMemLimit is initialized in the init() function from the
// EnvTestDockerMemLimit env variable.
var DockerMemLimit int // bytes

func init() {
	Logger = &stdlog{log: log.New(os.Stdout, "[watchdog test] ", log.LstdFlags|log.Lmsgprefix), debug: true}

	if l := os.Getenv(EnvTestDockerMemLimit); l != "" {
		l, err := strconv.Atoi(l)
		if err != nil {
			panic(err)
		}
		DockerMemLimit = l
	}
}

func skipIfNotIsolated(t *testing.T) {
	if os.Getenv(EnvTestIsolated) != "1" {
		t.Skipf("skipping test in non-isolated mode")
	}
}

var (
	limit64MiB uint64 = 64 << 20 // 64MiB.
)

func TestControl_Isolated(t *testing.T) {
	skipIfNotIsolated(t)

	debug.SetGCPercent(100)

	rounds := 100
	if DockerMemLimit != 0 {
		rounds /= int(float64(DockerMemLimit)*0.8) / 1024 / 1024
	}

	// retain 1MiB every iteration.
	var retained [][]byte
	for i := 0; i < rounds; i++ {
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
	require.NotZero(t, ms.NumGC)    // GCs have taken place, but...
	require.Zero(t, ms.NumForcedGC) // ... no forced GCs beyond our initial one.
}

func TestHeapDriven_Isolated(t *testing.T) {
	skipIfNotIsolated(t)

	// we can't mock ReadMemStats, because we're relying on the go runtime to
	// enforce the GC run, and the go runtime won't use our mock. Therefore, we
	// need to do the actual thing.
	debug.SetGCPercent(100)

	clk := clock.NewMock()
	Clock = clk

	observations := make([]*runtime.MemStats, 0, 100)
	NotifyGC = func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		observations = append(observations, &ms)
	}

	// limit is 64MiB.
	err, stopFn := HeapDriven(limit64MiB, 0, NewAdaptivePolicy(0.5))
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
	require.GreaterOrEqual(t, ms.NumGC, uint32(5)) // over 5 GCs should've taken place.
}

func TestSystemDriven_Isolated(t *testing.T) {
	skipIfNotIsolated(t)

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
	err, stopFn := SystemDriven(limit64MiB, 5*time.Second, NewAdaptivePolicy(0.5))
	require.NoError(t, err)
	defer stopFn()

	time.Sleep(200 * time.Millisecond) // give time for the watchdog to init.

	notifyChDeprecated := make(chan struct{}, 1)
	notifyCh := make(chan struct{}, 1)
	NotifyGC = func() {
		notifyChDeprecated <- struct{}{}
	}
	RegisterNotifee("test notifee", func() {
		notifyCh <- struct{}{}
	})

	// first tick; used = 0.
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyChDeprecated, 0) // no GC has taken place.
	require.Len(t, notifyCh, 0)           // no GC has taken place.

	// second tick; used = just over 50%; will trigger GC.
	actualUsed = (limit64MiB / 2) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyChDeprecated, 1)
	require.Len(t, notifyCh, 1)
	<-notifyChDeprecated
	<-notifyCh

	// third tick; just below 75%; no GC.
	actualUsed = uint64(float64(limit64MiB)*0.75) - 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyChDeprecated, 0)
	require.Len(t, notifyCh, 0)

	// fourth tick; 75% exactly; will trigger GC.
	actualUsed = uint64(float64(limit64MiB)*0.75) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, notifyCh, 1)
	require.Len(t, notifyChDeprecated, 1)
	<-notifyChDeprecated
	<-notifyCh

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	require.GreaterOrEqual(t, ms.NumForcedGC, uint32(2))
}

// TestHeapdumpCapture tests that heap dumps are captured appropriately.
func TestHeapdumpCapture(t *testing.T) {
	debug.SetGCPercent(100)

	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	assertFileCount := func(expected int) {
		glob, err := filepath.Glob(filepath.Join(dir, "*"))
		require.NoError(t, err)
		require.Len(t, glob, expected)
	}

	HeapProfileDir = dir
	HeapProfileThreshold = 0.5
	HeapProfileMaxCaptures = 5

	// mock clock.
	clk := clock.NewMock()
	Clock = clk

	// mock the system reporting.
	var actualUsed uint64
	sysmemFn = func(g *gosigar.Mem) error {
		g.ActualUsed = actualUsed
		return nil
	}

	// init a system driven watchdog.
	err, stopFn := SystemDriven(limit64MiB, 5*time.Second, NewAdaptivePolicy(0.5))
	require.NoError(t, err)
	defer stopFn()
	time.Sleep(200 * time.Millisecond) // give time for the watchdog to init.

	// first tick; used = 0.
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	assertFileCount(0)

	// second tick; used = just over 50%; will trigger a heapdump.
	actualUsed = (limit64MiB / 2) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	assertFileCount(1)

	// third tick; continues above 50%; same episode, no heapdump.
	actualUsed = (limit64MiB / 2) + 10
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	assertFileCount(1)

	// fourth tick; below 50%; this resets the episodic flag.
	actualUsed = limit64MiB / 3
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	assertFileCount(1)

	// fifth tick; above 50%; this triggers a new heapdump.
	actualUsed = (limit64MiB / 2) + 1
	clk.Add(5 * time.Second)
	time.Sleep(200 * time.Millisecond)
	assertFileCount(2)

	for i := 0; i < 20; i++ {
		// below 50%; this resets the episodic flag.
		actualUsed = limit64MiB / 3
		clk.Add(5 * time.Second)
		time.Sleep(200 * time.Millisecond)

		// above 50%; this triggers a new heapdump.
		actualUsed = (limit64MiB / 2) + 1
		clk.Add(5 * time.Second)
		time.Sleep(200 * time.Millisecond)
	}

	assertFileCount(5) // we only generated 5 heap dumps even though we had more episodes.

	// verify that heap dump file sizes aren't zero.
	glob, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	for _, f := range glob {
		fi, err := os.Stat(f)
		require.NoError(t, err)
		require.NotZero(t, fi.Size())
	}
}

type panickingPolicy struct{}

func (panickingPolicy) Evaluate(_ UtilizationType, _ uint64) (_ uint64) {
	panic("oops!")
}

func TestPanicRecover(t *testing.T) {
	// replace the logger with one that tees into the buffer.
	b := new(bytes.Buffer)
	Logger.(*stdlog).log.SetOutput(io.MultiWriter(b, os.Stdout))

	// simulate a polling watchdog with a panicking policy.
	_watchdog.wg.Add(1)
	pollingWatchdog(panickingPolicy{}, 1*time.Millisecond, 10000000, func() (uint64, error) {
		return 0, nil
	})
	require.Contains(t, b.String(), "WATCHDOG PANICKED")

	b.Reset() // reset buffer.
	require.NotContains(t, b.String(), "WATCHDOG PANICKED")

	// simulate a polling watchdog with a panicking usage.
	_watchdog.wg.Add(1)
	pollingWatchdog(&adaptivePolicy{factor: 0.5}, 1*time.Millisecond, 10000000, func() (uint64, error) {
		panic("bang!")
	})
	require.Contains(t, b.String(), "WATCHDOG PANICKED")
}
