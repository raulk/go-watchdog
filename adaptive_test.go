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
	next := p.Evaluate(UtilizationSystem, 0)
	require.EqualValues(t, limit/2, next)

	// at half; next = 75%.
	next = p.Evaluate(UtilizationSystem, limit/2)
	require.EqualValues(t, 3*(limit/4), next)

	// at limit.
	next = p.Evaluate(UtilizationSystem, limit)
	require.EqualValues(t, limit, next)
}
