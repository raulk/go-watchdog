package watchdog

import (
	"math"
	"time"
)

// WatermarkPolicy is a watchdog firing policy that triggers when watermarks are
// surpassed in the increasing direction.
//
// For example, a policy configured with the watermarks 0.50, 0.75, 0.80, and
// 0.99 will trigger at most once, and only once, each time that a watermark
// is surpassed upwards.
//
// Even if utilisation pierces through several watermarks at once between two
// subsequent calls to Evaluate, the policy will fire only once.
//
// It is possible to suppress the watermark policy from firing too often by
// setting a Silence period. When non-zero, if a watermark is surpassed within
// the Silence window (using the last GC timestamp as basis), that event will
// not immediately trigger firing. Instead, the policy will wait until the
// silence period is over, and only then will it fire if utilisation is still
// beyond that watermark.
//
// At last, if an EmergencyWatermark is set, when utilisation is above that
// level, the Silence period will be ignored and the policy will fire
// persistenly, as long as utilisation stays above that watermark.
type WatermarkPolicy struct {
	// Watermarks are the percentual amounts of limit. The policy will panic if
	// Watermarks is zero length.
	Watermarks []float64

	// EmergencyWatermark is a watermark that, when surpassed, puts this
	// watchdog in emergency mode. During emergency mode, the system is
	// considered to be under significant memory pressure, and the Quiesce
	// period is not honoured.
	EmergencyWatermark float64

	// Silence is the quiet period the watchdog will honour except when in
	// emergency mode.
	Silence time.Duration

	// internal state.
	thresholds  []uint64
	currIdx     int // idx of the current watermark.
	lastIdx     int
	firedLast   bool
	silenceNs   int64
	initialized bool
}

var _ Policy = (*WatermarkPolicy)(nil)

func (w *WatermarkPolicy) Evaluate(input PolicyInput) (trigger bool) {
	if !w.initialized {
		w.thresholds = make([]uint64, 0, len(w.Watermarks))
		for _, m := range w.Watermarks {
			w.thresholds = append(w.thresholds, uint64(float64(input.Limit)*m))
		}
		w.silenceNs = w.Silence.Nanoseconds()
		w.lastIdx = len(w.Watermarks) - 1
		w.initialized = true
		input.Logger.Infof("initialized watermark watchdog policy; watermarks: %v; emergency watermark: %f, thresholds: %v; silence period: %s",
			w.Watermarks, w.EmergencyWatermark, w.thresholds, w.Silence)
	}

	// determine the value to compare utilisation against.
	var actual uint64
	switch input.Scope {
	case ScopeSystem:
		actual = input.SysStats.ActualUsed
	case ScopeHeap:
		actual = input.MemStats.HeapAlloc
	}

	input.Logger.Debugf("watermark policy: evaluating; curr_watermark: %f, utilization: %d/%d/%d (used/curr_threshold/limit)",
		w.Watermarks[w.currIdx], actual, w.thresholds[w.currIdx], input.Limit)

	// determine whether we're past the emergency watermark; if we are, we fire
	// unconditionally. Disabled if 0.
	if w.EmergencyWatermark > 0 {
		currPercentage := math.Round((float64(actual)/float64(input.Limit))*DecimalPrecision) / DecimalPrecision // round float.
		if pastEmergency := currPercentage >= w.EmergencyWatermark; pastEmergency {
			emergencyThreshold := uint64(float64(input.Limit) / w.EmergencyWatermark)
			input.Logger.Infof("watermark policy: emergency trigger; perc: %f/%f (%% used/%% emergency), utilization: %d/%d/%d (used/emergency/limit), used: %d, limit: %d, ",
				currPercentage, w.EmergencyWatermark, actual, emergencyThreshold, input.Limit)
			return true
		}
	}

	// short-circuit if within the silencing period.
	if silencing := w.silenceNs > 0; silencing {
		now := Clock.Now().UnixNano()
		if elapsed := now - int64(input.MemStats.LastGC); elapsed < w.silenceNs {
			input.Logger.Debugf("watermark policy: silenced")
			return false
		}
	}

	// check if the the utilisation is below our current threshold; try
	// to downscale before returning false.
	if actual < w.thresholds[w.currIdx] {
		for w.currIdx > 0 {
			if actual >= w.thresholds[w.currIdx-1] {
				break
			}
			w.firedLast = false
			w.currIdx--
		}
		return false
	}

	// we are above our current watermark, but if this is the last watermark and
	// we've already fired, suppress the firing.
	if w.currIdx == w.lastIdx && w.firedLast {
		input.Logger.Debugf("watermark policy: last watermark already fired; skipping")
		return false
	}

	// if our value is above the last threshold, record the last threshold as
	// our current watermark, and also the fact that we've already fired for
	// it.
	if actual >= w.thresholds[w.lastIdx] {
		w.currIdx = w.lastIdx
		w.firedLast = true
		input.Logger.Infof("watermark policy triggering: %f watermark surpassed", w.Watermarks[w.currIdx])
		return true
	}

	input.Logger.Infof("watermark policy triggering: %f watermark surpassed", w.Watermarks[w.currIdx])

	// we are triggering; update the current watermark, upscaling it to the
	// next threshold.
	for w.currIdx < w.lastIdx {
		w.currIdx++
		if actual < w.thresholds[w.currIdx] {
			break
		}
	}

	return true
}
