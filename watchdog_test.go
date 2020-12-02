package watchdog

import (
	"runtime"
	"testing"
	"time"

	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

type testPolicy struct {
	ch      chan *PolicyInput
	trigger bool
}

var _ Policy = (*testPolicy)(nil)

func (t *testPolicy) Evaluate(input PolicyInput) (trigger bool) {
	t.ch <- &input
	return t.trigger
}

func TestWatchdog(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	tp := &testPolicy{ch: make(chan *PolicyInput, 1)}
	notifyCh := make(chan struct{}, 1)

	err, stop := Memory(MemConfig{
		Scope:       ScopeHeap,
		Limit:       100,
		Resolution:  10 * time.Second,
		Policy:      tp,
		NotifyFired: func() { notifyCh <- struct{}{} },
	})
	require.NoError(t, err)
	defer stop()

	time.Sleep(500 * time.Millisecond) // wait til the watchdog goroutine starts.

	clk.Add(10 * time.Second)
	pi := <-tp.ch
	require.EqualValues(t, 100, pi.Limit)
	require.NotNil(t, pi.MemStats)
	require.NotNil(t, pi.SysStats)
	require.Equal(t, ScopeHeap, pi.Scope)

	require.Len(t, notifyCh, 0)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	require.EqualValues(t, 0, ms.NumGC)

	// now fire.
	tp.trigger = true

	clk.Add(10 * time.Second)
	pi = <-tp.ch
	require.EqualValues(t, 100, pi.Limit)
	require.NotNil(t, pi.MemStats)
	require.NotNil(t, pi.SysStats)
	require.Equal(t, ScopeHeap, pi.Scope)

	time.Sleep(500 * time.Millisecond) // wait until the watchdog runs.

	require.Len(t, notifyCh, 1)

	runtime.ReadMemStats(&ms)
	require.EqualValues(t, 1, ms.NumGC)
}

func TestDoubleClose(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatal(r)
		}
	}()

	tp := &testPolicy{ch: make(chan *PolicyInput, 1)}

	err, stop := Memory(MemConfig{
		Scope:      ScopeHeap,
		Limit:      100,
		Resolution: 10 * time.Second,
		Policy:     tp,
	})
	require.NoError(t, err)
	stop()
	stop()
}
