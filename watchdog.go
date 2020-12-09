package watchdog

import (
	"fmt"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
)

// The watchdog is designed to be used as a singleton; global vars are OK for
// that reason.
var (
	// Logger is the logger to use. If nil, it will default to a logger that
	// proxies to a standard logger using the "[watchdog]" prefix.
	Logger logger = &stdlog{log: log.New(log.Writer(), "[watchdog] ", log.LstdFlags|log.Lmsgprefix)}

	// Clock can be used to inject a mock clock for testing.
	Clock = clock.New()

	// NotifyFired, if non-nil, will be called when the policy has fired,
	// prior to calling GC, even if GC is disabled.
	NotifyFired func() = func() {}
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
	//
	// The policy can request immediate GC, in which case next should match the
	// used memory.
	Evaluate(scope UtilizationType, used uint64) (next uint64, immediate bool)
}

// HeapDriven starts a singleton heap-driven watchdog.
//
// The heap-driven watchdog adjusts GOGC dynamically after every GC, to honour
// the policy. When an immediate GC is requested, runtime.GC() is called, and
// the policy is re-evaluated at the end of GC.
//
// It is entirely possible for the policy to keep requesting immediate GC
// repeateadly. This usually signals an emergency situation, and won't prevent
// the program from making progress, since the Go's garbage collection is not
// stop-the-world (for the major part).
//
// A limit value of 0 will error.
func HeapDriven(limit uint64, policyCtor PolicyCtor) (err error, stopFn func()) {
	_watchdog.lk.Lock()
	defer _watchdog.lk.Unlock()

	if _watchdog.state != stateUnstarted {
		return ErrAlreadyStarted, nil
	}

	if limit == 0 {
		return fmt.Errorf("cannot use zero limit for heap-driven watchdog"), nil
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	_watchdog.state = stateRunning
	_watchdog.scope = UtilizationHeap
	_watchdog.closing = make(chan struct{})

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
				NotifyFired()

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
			next, immediate := policy.Evaluate(UtilizationHeap, memstats.HeapAlloc)

			if immediate {
				// trigger a forced GC; because we're not making the finalizer
				// skip sending to the trigger channel, we will get fired again.
				// at this stage, the program is under significant pressure, and
				// given that Go GC is not STW for the largest part, the worse
				// thing that could happen from infinitely GC'ing is that the
				// program will run in a degrated state for longer, possibly
				// long enough for an operator to intervene.
				Logger.Warnf("heap-driven watchdog requested immediate GC; " +
					"system is probably under significant pressure; " +
					"performance compromised")
				forceGC(&memstats)
				continue
			}

			// calculate how much to set GOGC to honour the next trigger point.
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
	_watchdog.lk.Lock()
	defer _watchdog.lk.Unlock()

	if _watchdog.state != stateUnstarted {
		return ErrAlreadyStarted, nil
	}

	if limit == 0 {
		limit, err = determineLimit(false)
		if err != nil {
			return err, nil
		}
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	_watchdog.state = stateRunning
	_watchdog.scope = UtilizationSystem
	_watchdog.closing = make(chan struct{})

	gcTriggered := make(chan struct{}, 16)
	setupGCSentinel(gcTriggered)

	_watchdog.wg.Add(1)
	go func() {
		defer _watchdog.wg.Done()

		var (
			memstats  runtime.MemStats
			sysmem    gosigar.Mem
			threshold uint64
		)

		// initialize the threshold.
		threshold, immediate := policy.Evaluate(UtilizationSystem, sysmem.ActualUsed)
		if immediate {
			Logger.Warnf("system-driven watchdog requested immediate GC upon startup; " +
				"policy is probably misconfigured; " +
				"performance compromised")
			forceGC(&memstats)
		}

		for {
			select {
			case <-Clock.After(frequency):
				// get the current usage.
				if err := sysmemFn(&sysmem); err != nil {
					Logger.Warnf("failed to obtain system memory stats; err: %s", err)
					continue
				}
				actual := sysmem.ActualUsed
				if actual < threshold {
					// nothing to do.
					continue
				}
				// trigger GC; this will emit a gcTriggered event which we'll
				// consume next to readjust the threshold.
				Logger.Warnf("system-driven watchdog triggering GC; %d/%d bytes (used/threshold)", actual, threshold)
				forceGC(&memstats)

			case <-gcTriggered:
				NotifyFired()

				// get the current usage.
				if err := sysmemFn(&sysmem); err != nil {
					Logger.Warnf("failed to obtain system memory stats; err: %s", err)
					continue
				}

				// adjust the threshold.
				threshold, immediate = policy.Evaluate(UtilizationSystem, sysmem.ActualUsed)
				if immediate {
					Logger.Warnf("system-driven watchdog triggering immediate GC; %d used bytes", sysmem.ActualUsed)
					forceGC(&memstats)
				}

			case <-_watchdog.closing:
				return
			}
		}
	}()

	return nil, stop
}

func determineLimit(restrictByProcess bool) (uint64, error) {
	// TODO.
	// if restrictByProcess {
	// 	if pmem := ProcessMemoryLimit(); pmem > 0 {
	// 		Logger.Infof("watchdog using process limit: %d bytes", pmem)
	// 		return pmem, nil
	// 	}
	// 	Logger.Infof("watchdog was unable to determine process limit; falling back to total system memory")
	// }

	// populate initial utilisation and system stats.
	var sysmem gosigar.Mem
	if err := sysmemFn(&sysmem); err != nil {
		return 0, fmt.Errorf("failed to get system memory stats: %w", err)
	}
	return sysmem.Total, nil
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
