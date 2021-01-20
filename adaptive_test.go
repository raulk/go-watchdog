package watchdog

import (
	"testing"

	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

func TestAdaptivePolicy(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p, err := NewAdaptivePolicy(0.5)(limit64MiB)
	require.NoError(t, err)

	// at zero; next = 50%.
	next := p.Evaluate(UtilizationSystem, 0)
	require.EqualValues(t, limit64MiB/2, next)

	// at half; next = 75%.
	next = p.Evaluate(UtilizationSystem, limit64MiB/2)
	require.EqualValues(t, 3*(limit64MiB/4), next)

	// at limit.
	next = p.Evaluate(UtilizationSystem, limit64MiB)
	require.EqualValues(t, limit64MiB, next)
}
