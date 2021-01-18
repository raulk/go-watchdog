package watchdog

import (
	"errors"
	"fmt"
	"log"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
)

// ErrNotSupported is returned when the watchdog does not support the requested
// run mode in the current OS/arch.
var ErrNotSupported = errors.New("watchdog run mode not supported")

// PolicyTempDisabled is a marker value for policies to signal that the policy
// is temporarily disabled. Use it when all hope is lost to turn around from
// significant memory pressure (such as when above an "extreme" watermark).
const PolicyTempDisabled uint64 = math.MaxUint64

// The watchdog is designed to be used as a singleton; global vars are OK for
// that reason.
var (
	// Logger is the logger to use. If nil, it will default to a logger that
	// proxies to a standard logger using the "[watchdog]" prefix.
	Logger logger = &stdlog{log: log.New(log.Writer(), "[watchdog] ", log.LstdFlags|log.Lmsgprefix)}

	// Clock can be used to inject a mock clock for testing.
	Clock = clock.New()

	// NotifyGC, if non-nil, will be called when a GC has happened.
	NotifyGC func() = func() {}
)

var (
	// ReadMemStats stops the world. But as of go1.9, it should only
	// take ~25µs to complete.
	//
	// Before go1.15, calls to ReadMemStats during an ongoing GC would
	// block due to the worldsema lock. As of go1.15, this was optimized
	// and the runtime holds on to worldsema less during GC (only during
	// sweep termination and mark termination).
	//
	// For users using go1.14 and earlier, if this call happens during
	// GC, it will just block for longer until serviced, but it will not
	// take longer in itself. No harm done.
	//
	// Actual benchmarks
	// -----------------
	//
	// In Go 1.15.5, ReadMem with no ongoing GC takes ~27µs in a MBP 16
	// i9 busy with another million things. During GC, it takes an
	// average of less than 175µs per op.
	//
	// goos: darwin
	// goarch: amd64
	// pkg: github.com/filecoin-project/lotus/api
	// BenchmarkReadMemStats-16                	   44530	     27523 ns/op
	// BenchmarkReadMemStats-16                	   43743	     26879 ns/op
	// BenchmarkReadMemStats-16                	   45627	     26791 ns/op
	// BenchmarkReadMemStats-16                	   44538	     26219 ns/op
	// BenchmarkReadMemStats-16                	   44958	     26757 ns/op
	// BenchmarkReadMemStatsWithGCContention-16    	      10	    183733 p50-ns	    211859 p90-ns	    211859 p99-ns
	// BenchmarkReadMemStatsWithGCContention-16    	       7	    198765 p50-ns	    314873 p90-ns	    314873 p99-ns
	// BenchmarkReadMemStatsWithGCContention-16    	      10	    195151 p50-ns	    311408 p90-ns	    311408 p99-ns
	// BenchmarkReadMemStatsWithGCContention-16    	      10	    217279 p50-ns	    295308 p90-ns	    295308 p99-ns
	// BenchmarkReadMemStatsWithGCContention-16    	      10	    167054 p50-ns	    327072 p90-ns	    327072 p99-ns
	// PASS
	//
	// See: https://github.com/golang/go/issues/19812
	// See: https://github.com/prometheus/client_golang/issues/403
	memstatsFn = runtime.ReadMemStats
	sysmemFn   = (*gosigar.Mem).Get
)

var (
	// ErrAlreadyStarted is returned when the user tries to start the watchdog more than once.
	ErrAlreadyStarted = fmt.Errorf("singleton memory watchdog was already started")
)

const (
	// stateUnstarted represents an unstarted state.
	stateUnstarted int32 = iota
	// stateRunning represents an operational state.
	stateRunning
)

// _watchdog is a global singleton watchdog.
var _watchdog struct {
	lk    sync.Mutex
	state int32

	scope UtilizationType

	closing chan struct{}
	wg      sync.WaitGroup
}

// UtilizationType is the utilization metric in use.
type UtilizationType int

const (
	// UtilizationSystem specifies that the policy compares against actual used
	// system memory.
	UtilizationSystem UtilizationType = iota
	// UtilizationProcess specifies that the watchdog is using process limits.
	UtilizationProcess
	// UtilizationHeap specifies that the policy compares against heap used.
	UtilizationHeap
)

// PolicyCtor is a policy constructor.
type PolicyCtor func(limit uint64) (Policy, error)

// Policy is polled by the watchdog to determine the next utilisation at which
// a GC should be forced.
type Policy interface {
	// Evaluate determines when the next GC should take place. It receives the
	// current usage, and it returns the next usage at which to trigger GC.
	Evaluate(scope UtilizationType, used uint64) (next uint64)
}

// HeapDriven starts a singleton heap-driven watchdog.
//
// The heap-driven watchdog adjusts GOGC dynamically after every GC, to honour
// the policy requirements.
//
// A zero-valued limit will error.
func HeapDriven(limit uint64, policyCtor PolicyCtor) (err error, stopFn func()) {
	if limit == 0 {
		return fmt.Errorf("cannot use zero limit for heap-driven watchdog"), nil
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	if err := start(UtilizationHeap); err != nil {
		return err, nil
	}

	gcTriggered := make(chan struct{}, 16)
	setupGCSentinel(gcTriggered)

	_watchdog.wg.Add(1)
	go func() {
		defer _watchdog.wg.Done()

		// get the initial effective GOGC; guess it's 100 (default), and restore
		// it to whatever it actually was. This works because SetGCPercent
		// returns the previous value.
		originalGOGC := debug.SetGCPercent(debug.SetGCPercent(100))
		currGOGC := originalGOGC

		var memstats runtime.MemStats
		for {
			select {
			case <-gcTriggered:
				NotifyGC()

			case <-_watchdog.closing:
				return
			}

			// recompute the next trigger.
			memstatsFn(&memstats)

			// heapMarked is the amount of heap that was marked as live by GC.
			// it is inferred from our current GOGC and the new target picked.
			heapMarked := uint64(float64(memstats.NextGC) / (1 + float64(currGOGC)/100))
			if heapMarked == 0 {
				// this shouldn't happen, but just in case; avoiding a div by 0.
				Logger.Warnf("heap-driven watchdog: inferred zero heap marked; skipping evaluation")
				continue
			}

			// evaluate the policy.
			next := policy.Evaluate(UtilizationHeap, memstats.HeapAlloc)

			// calculate how much to set GOGC to honour the next trigger point.
			// next=PolicyTempDisabled value would make currGOGC extremely high,
			// greater than originalGOGC, and therefore we'd restore originalGOGC.
			currGOGC = int(((float64(next) / float64(heapMarked)) - float64(1)) * 100)
			if currGOGC >= originalGOGC {
				Logger.Debugf("heap watchdog: requested GOGC percent higher than default; capping at default; requested: %d; default: %d", currGOGC, originalGOGC)
				currGOGC = originalGOGC
			} else {
				if currGOGC < 1 {
					currGOGC = 1
				}
				Logger.Infof("heap watchdog: setting GOGC percent: %d", currGOGC)
			}

			debug.SetGCPercent(currGOGC)

			memstatsFn(&memstats)
			Logger.Infof("heap watchdog stats: heap_alloc: %d, heap_marked: %d, next_gc: %d, policy_next_gc: %d, gogc: %d",
				memstats.HeapAlloc, heapMarked, memstats.NextGC, next, currGOGC)
		}
	}()

	return nil, stop
}

// SystemDriven starts a singleton system-driven watchdog.
//
// The system-driven watchdog keeps a threshold, above which GC will be forced.
// The watchdog polls the system utilization at the specified frequency. When
// the actual utilization exceeds the threshold, a GC is forced.
//
// This threshold is calculated by querying the policy every time that GC runs,
// either triggered by the runtime, or forced by us.
func SystemDriven(limit uint64, frequency time.Duration, policyCtor PolicyCtor) (err error, stopFn func()) {
	if limit == 0 {
		var sysmem gosigar.Mem
		if err := sysmemFn(&sysmem); err != nil {
			return fmt.Errorf("failed to get system memory stats: %w", err), nil
		}
		limit = sysmem.Total
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	if err := start(UtilizationSystem); err != nil {
		return err, nil
	}

	_watchdog.wg.Add(1)
	var sysmem gosigar.Mem
	go pollingWatchdog(policy, frequency, func() (uint64, error) {
		if err := sysmemFn(&sysmem); err != nil {
			return 0, err
		}
		return sysmem.ActualUsed, nil
	})

	return nil, stop
}

// pollingWatchdog starts a polling watchdog with the provided policy, using
// the supplied polling frequency. On every tick, it calls usageFn and, if the
// usage is greater or equal to the threshold at the time, it forces GC.
// usageFn is guaranteed to be called serially, so no locking should be
// necessary.
func pollingWatchdog(policy Policy, frequency time.Duration, usageFn func() (uint64, error)) {
	defer _watchdog.wg.Done()

	gcTriggered := make(chan struct{}, 16)
	setupGCSentinel(gcTriggered)

	var (
		memstats  runtime.MemStats
		threshold uint64
	)

	renewThreshold := func() {
		// get the current usage.
		usage, err := usageFn()
		if err != nil {
			Logger.Warnf("failed to obtain memory utilization stats; err: %s", err)
			return
		}
		// calculate the threshold.
		threshold = policy.Evaluate(_watchdog.scope, usage)
	}

	// initialize the threshold.
	renewThreshold()

	// initialize an empty timer.
	timer := Clock.Timer(0)
	stopTimer := func() {
		if !timer.Stop() {
			<-timer.C
		}
	}

	for {
		timer.Reset(frequency)

		select {
		case <-timer.C:
			// get the current usage.
			usage, err := usageFn()
			if err != nil {
				Logger.Warnf("failed to obtain memory utilizationstats; err: %s", err)
				continue
			}
			if usage < threshold {
				// nothing to do.
				continue
			}
			// trigger GC; this will emit a gcTriggered event which we'll
			// consume next to readjust the threshold.
			Logger.Warnf("system-driven watchdog triggering GC; %d/%d bytes (used/threshold)", usage, threshold)
			forceGC(&memstats)

		case <-gcTriggered:
			NotifyGC()

			renewThreshold()

			stopTimer()

		case <-_watchdog.closing:
			stopTimer()
			return
		}
	}
}

// forceGC forces a manual GC.
func forceGC(memstats *runtime.MemStats) {
	Logger.Infof("watchdog is forcing GC")

	// it's safe to assume that the finalizer will attempt to run before
	// runtime.GC() returns because runtime.GC() waits for the sweep phase to
	// finish before returning.
	// finalizers are run in the sweep phase.
	start := time.Now()
	runtime.GC()
	took := time.Since(start)

	memstatsFn(memstats)
	Logger.Infof("watchdog-triggered GC finished; took: %s; current heap allocated: %d bytes", took, memstats.HeapAlloc)
}

func setupGCSentinel(gcTriggered chan struct{}) {
	logger := Logger

	// this non-zero sized struct is used as a sentinel to detect when a GC
	// run has finished, by setting and resetting a finalizer on it.
	// it essentially creates a GC notification "flywheel"; every GC will
	// trigger this finalizer, which will reset itself so it gets notified
	// of the next GC, breaking the cycle when the watchdog is stopped.
	type sentinel struct{ a *int }
	var finalizer func(o *sentinel)
	finalizer = func(o *sentinel) {
		_watchdog.lk.Lock()
		defer _watchdog.lk.Unlock()

		if _watchdog.state != stateRunning {
			// this GC triggered after the watchdog was stopped; ignore
			// and do not reset the finalizer.
			return
		}

		// reset so it triggers on the next GC.
		runtime.SetFinalizer(o, finalizer)

		select {
		case gcTriggered <- struct{}{}:
		default:
			logger.Warnf("failed to queue gc trigger; channel backlogged")
		}
	}

	runtime.SetFinalizer(&sentinel{}, finalizer) // start the flywheel.
}

func start(scope UtilizationType) error {
	_watchdog.lk.Lock()
	defer _watchdog.lk.Unlock()

	if _watchdog.state != stateUnstarted {
		return ErrAlreadyStarted
	}

	_watchdog.state = stateRunning
	_watchdog.scope = scope
	_watchdog.closing = make(chan struct{})
	return nil
}

func stop() {
	_watchdog.lk.Lock()
	defer _watchdog.lk.Unlock()

	if _watchdog.state != stateRunning {
		return
	}

	close(_watchdog.closing)
	_watchdog.wg.Wait()
	_watchdog.state = stateUnstarted
}
