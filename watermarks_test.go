package watchdog

import (
	"testing"

	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

var (
	watermarks = []float64{0.50, 0.75, 0.80}
	thresholds = func() []uint64 {
		var ret []uint64
		for _, w := range watermarks {
			ret = append(ret, uint64(float64(limit64MiB)*w))
		}
		return ret
	}()
)

func TestProgressiveWatermarks(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p, err := NewWatermarkPolicy(watermarks...)(limit64MiB)
	require.NoError(t, err)

	// at zero
	next := p.Evaluate(UtilizationSystem, uint64(0))
	require.EqualValues(t, thresholds[0], next)

	// before the watermark.
	next = p.Evaluate(UtilizationSystem, uint64(float64(limit64MiB)*watermarks[0])-1)
	require.EqualValues(t, thresholds[0], next)

	// exactly at the watermark; gives us the next watermark, as the watchdodg would've
	// taken care of triggering the first watermark.
	next = p.Evaluate(UtilizationSystem, uint64(float64(limit64MiB)*watermarks[0]))
	require.EqualValues(t, thresholds[1], next)

	// after the watermark gives us the next watermark.
	next = p.Evaluate(UtilizationSystem, uint64(float64(limit64MiB)*watermarks[0])+1)
	require.EqualValues(t, thresholds[1], next)

	// last watermark; disable the policy.
	next = p.Evaluate(UtilizationSystem, uint64(float64(limit64MiB)*watermarks[2]))
	require.EqualValues(t, PolicyTempDisabled, next)

	next = p.Evaluate(UtilizationSystem, uint64(float64(limit64MiB)*watermarks[2]+1))
	require.EqualValues(t, PolicyTempDisabled, next)

	next = p.Evaluate(UtilizationSystem, limit64MiB)
	require.EqualValues(t, PolicyTempDisabled, next)
}
