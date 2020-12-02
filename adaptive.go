package watchdog

// AdaptivePolicy is a policy that forces GC when the usage surpasses the
// available memory after the last GC run.
//
// TODO tests
type AdaptivePolicy struct {
	// Factor determines how much this policy will let the heap expand
	// before it triggers.
	//
	// On every GC run, this policy recalculates the next target as
	// (limit-currentHeap)*Factor (i.e. available*Factor).
	//
	// If the GC target calculated by the runtime is lower than the one
	// calculated by this policy, this policy will set the new target, but the
	// effect will be nil, since the the go runtime will run GC sooner than us
	// anyway.
	Factor float64

	active      bool
	target      uint64
	initialized bool
}

var _ Policy = (*AdaptivePolicy)(nil)

func (a *AdaptivePolicy) Evaluate(input PolicyInput) (trigger bool) {
	if !a.initialized {
		// when initializing, set the target to the limit; it will be reset
		// when the first GC happens.
		a.target = input.Limit
		a.initialized = true
	}

	// determine the value to compare utilisation against.
	var actual uint64
	switch input.Scope {
	case ScopeSystem:
		actual = input.SysStats.ActualUsed
	case ScopeHeap:
		actual = input.MemStats.HeapAlloc
	}

	if input.GCTrigger {
		available := float64(input.Limit) - float64(actual)
		calc := uint64(available * a.Factor)
		a.target = calc
	}
	if actual >= a.target {
		return true
	}
	return false
}
