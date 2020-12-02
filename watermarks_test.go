package watchdog

import (
	"log"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

var (
	limit              uint64 = 64 << 20 // 64MiB.
	firstWatermark            = 0.50
	secondWatermark           = 0.75
	thirdWatermark            = 0.80
	emergencyWatermark        = 0.90

	logger = &stdlog{log: log.New(os.Stdout, "[watchdog test] ", log.LstdFlags|log.Lmsgprefix), debug: true}
)

func TestProgressiveWatermarksSystem(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p := WatermarkPolicy{
		Watermarks:         []float64{firstWatermark, secondWatermark, thirdWatermark},
		EmergencyWatermark: emergencyWatermark,
	}

	require.False(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit)*firstWatermark) - 1},
	}))

	// trigger the first watermark.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(time.Now().UnixNano())},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * firstWatermark)},
	}))

	// this won't fire because we're still on the same watermark.
	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(time.Now().UnixNano())},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * firstWatermark)},
		}))
	}

	// now let's move to the second watermark.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(time.Now().UnixNano())},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * secondWatermark)},
	}))

	// this won't fire because we're still on the same watermark.
	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * secondWatermark)},
		}))
	}

	// now let's move to the third and last watermark.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(0)},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * thirdWatermark)},
	}))

	// this won't fire because we're still on the same watermark.
	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * thirdWatermark)},
		}))
	}
}

func TestProgressiveWatermarksHeap(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p := WatermarkPolicy{
		Watermarks: []float64{firstWatermark, secondWatermark, thirdWatermark},
	}

	// now back up step by step, check that all of them fire.
	for _, wm := range []float64{firstWatermark, secondWatermark, thirdWatermark} {
		require.True(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeHeap,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0), HeapAlloc: uint64(float64(limit) * wm)},
			SysStats: &gosigar.Mem{ActualUsed: 0},
		}))
	}
}

func TestDownscalingWatermarks_Reentrancy(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p := WatermarkPolicy{
		Watermarks: []float64{firstWatermark, secondWatermark, thirdWatermark},
	}

	// crank all the way to the top.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(0)},
		SysStats: &gosigar.Mem{ActualUsed: limit},
	}))

	// now back down, checking that none of the boundaries fire.
	for _, wm := range []float64{thirdWatermark, secondWatermark, firstWatermark} {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit)*wm - 1)},
		}))
	}

	// now back up step by step, check that all of them fire.
	for _, wm := range []float64{firstWatermark, secondWatermark, thirdWatermark} {
		require.True(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit)*wm + 1)},
		}))
	}

	// check the top does not fire because it already fired.
	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: limit},
		}))
	}
}

func TestEmergencyWatermark(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p := WatermarkPolicy{
		Watermarks:         []float64{firstWatermark},
		EmergencyWatermark: emergencyWatermark,
		Silence:            1 * time.Minute,
	}

	// every tick triggers, even within the silence period.
	for i := 0; i < 100; i++ {
		require.True(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(clk.Now().UnixNano())},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * emergencyWatermark)},
		}))
	}
}

func TestJumpWatermark(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p := WatermarkPolicy{
		Watermarks: []float64{firstWatermark, secondWatermark, thirdWatermark},
	}

	// check that jumping to the top only fires once.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(0)},
		SysStats: &gosigar.Mem{ActualUsed: limit},
	}))

	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(0)},
			SysStats: &gosigar.Mem{ActualUsed: limit},
		}))
	}

}

func TestSilencePeriod(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	var (
		limit           uint64 = 64 << 20 // 64MiB.
		firstWatermark         = 0.50
		secondWatermark        = 0.75
		thirdWatermark         = 0.80
	)

	p := WatermarkPolicy{
		Watermarks: []float64{firstWatermark, secondWatermark, thirdWatermark},
		Silence:    1 * time.Minute,
	}

	// going above the first threshold, but within silencing period, so nothing happens.
	for i := 0; i < 100; i++ {
		require.False(t, p.Evaluate(PolicyInput{
			Logger:   logger,
			Scope:    ScopeSystem,
			Limit:    limit,
			MemStats: &runtime.MemStats{LastGC: uint64(clk.Now().UnixNano())},
			SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * firstWatermark)},
		}))
	}

	// now outside the silencing period, we do fire.
	clk.Add(time.Minute)

	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: 0},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * firstWatermark)},
	}))

	// but not the second time.
	require.False(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: 0},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * firstWatermark)},
	}))

	// now let's go up inside the silencing period, nothing happens.
	require.False(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(clk.Now().UnixNano())},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * secondWatermark)},
	}))

	// same thing, outside the silencing period should trigger.
	require.True(t, p.Evaluate(PolicyInput{
		Logger:   logger,
		Scope:    ScopeSystem,
		Limit:    limit,
		MemStats: &runtime.MemStats{LastGC: uint64(0)},
		SysStats: &gosigar.Mem{ActualUsed: uint64(float64(limit) * secondWatermark)},
	}))
}
