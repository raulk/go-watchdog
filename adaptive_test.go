package watchdog

import (
	"testing"

	"github.com/raulk/clock"
	"github.com/stretchr/testify/require"
)

func TestAdaptivePolicy(t *testing.T) {
	clk := clock.NewMock()
	Clock = clk

	p, err := NewAdaptivePolicy(0.5)(limit)
	require.NoError(t, err)

	// at zero; next = 50%.
	next, immediate := p.Evaluate(UtilizationSystem, 0)
	require.False(t, immediate)
	require.EqualValues(t, limit/2, next)

	// at half; next = 75%.
	next, immediate = p.Evaluate(UtilizationSystem, limit/2)
	require.False(t, immediate)
	require.EqualValues(t, 3*(limit/4), next)

	// at limit; immediate = true.
	next, immediate = p.Evaluate(UtilizationSystem, limit)
	require.True(t, immediate)
	require.EqualValues(t, limit, next)
}
