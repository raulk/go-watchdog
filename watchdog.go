package watchdog

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elastic/gosigar"
	"github.com/raulk/clock"
)

// DecimalPrecision is the rounding precision that float calculations will use.
// By default, 4 decimal places.
var DecimalPrecision = 1e4

// Clock can be used to inject a mock clock for testing.
var Clock = clock.New()

var (
	// ErrAlreadyStarted is returned when the user tries to start the watchdog more than once.
	ErrAlreadyStarted = fmt.Errorf("singleton memory watchdog was already started")
)

const (
	// stateUnstarted represents an unstarted state.
	stateUnstarted int32 = iota
	// stateRunning represents an operational state.
	stateRunning
	// stateClosing represents a temporary closing state.
	stateClosing
)

// _watchdog is a global singleton watchdog.
var _watchdog struct {
	state         int32 // guarded by atomic, one of state* constants.
	finalizerSkip int32 // guarded by atomic; signals to the sentinel finalizer that it should skip notifying.

	config MemConfig

	closing chan struct{}
	wg      sync.WaitGroup
}

// ScopeType defines the scope of the utilisation that we'll apply the limit to.
type ScopeType int

const (
	// ScopeSystem specifies that the policy compares against actual used
	// system memory.
	ScopeSystem ScopeType = iota
	// ScopeHeap specifies that the policy compares against heap used.
	ScopeHeap
)

// PolicyInput is the object that's passed to a policy when evaluating it.
//
type PolicyInput struct {
	Scope     ScopeType
	Limit     uint64
	MemStats  *runtime.MemStats
	SysStats  *gosigar.Mem
	GCTrigger bool // is this a GC trigger?
	Logger    Logger
}

// Policy encapsulates the logic that the watchdog will run on every tick.
type Policy interface {
	// Evaluate determines whether the policy should fire. It receives the
	// limit (either guessed or manually set), go runtime memory stats, and
	// system memory stats, amongst other things. It returns whether the policy
	// has fired or not.
	Evaluate(input PolicyInput) (trigger bool)
}

type MemConfig struct {
	// Scope is the scope at which the limit will be applied.
	Scope ScopeType

	// Limit is the memory available to this process. If zero, we will fall
	// back to querying the system total memory via SIGAR.
	Limit uint64

	// Resolution is the interval at which the watchdog will retrieve memory
	// stats and invoke the Policy.
	Resolution time.Duration

	// Policy sets the firing policy of this watchdog.
	Policy Policy

	// NotifyFired, if non-nil, will be called when the policy has fired,
	// prior to calling GC, even if GC is disabled.
	NotifyFired func()

	// NotifyOnly, if true, will cause the watchdog to only notify via the
	// callbacks, without triggering actual GC.
	NotifyOnly bool

	// Logger is the logger to use. If nil, it will default to a logger that
	// proxies to a standard logger using the "[watchdog]" prefix.
	Logger Logger
}

// Memory starts the singleton memory watchdog with the provided configuration.
func Memory(config MemConfig) (err error, stop func()) {
	if !atomic.CompareAndSwapInt32(&_watchdog.state, stateUnstarted, stateRunning) {
		return ErrAlreadyStarted, nil
	}

	if config.Logger == nil {
		config.Logger = &stdlog{log: log.New(log.Writer(), "[watchdog] ", log.LstdFlags|log.Lmsgprefix)}
	}

	// if the user didn't provide a limit, get the total memory.
	if config.Limit == 0 {
		var mem gosigar.Mem
		if err := mem.Get(); err != nil {
			return fmt.Errorf("failed to get system memory limit via SIGAR: %w", err), nil
		}
		config.Limit = mem.Total
	}

	_watchdog.config = config
	_watchdog.closing = make(chan struct{})

	_watchdog.wg.Add(1)
	go watch()

	return nil, stopMemory
}

func watch() {
	var (
		gcTriggered = make(chan struct{}, 16)

		memstats runtime.MemStats
		sysmem   gosigar.Mem
		config   = _watchdog.config
	)

	// this non-zero sized struct is used as a sentinel to detect when a GC
	// run has finished, by setting and resetting a finalizer on it.
	// it essentially creates a GC notification "flywheel"; every GC will
	// trigger this finalizer, which will reset itself so it gets notified
	// of the next GC, breaking the cycle when the watchdog is stopped.
	type sentinel struct{ a *int }
	var finalizer func(o *sentinel)
	finalizer = func(o *sentinel) {
		if atomic.LoadInt32(&_watchdog.state) != stateRunning {
			// this GC triggered after the watchdog was stopped; ignore
			// and do not reset the finalizer.
			return
		}

		// reset so it triggers on the next GC.
		runtime.SetFinalizer(o, finalizer)

		if atomic.LoadInt32(&_watchdog.finalizerSkip) == 1 {
			return // requested a skip.
		}

		select {
		case gcTriggered <- struct{}{}:
		default:
			config.Logger.Warnf("failed to queue gc trigger; channel backlogged")
		}
	}

	finalizer(&sentinel{}) // start the flywheel.

	defer func() {
		_watchdog.wg.Done()
	}()
	for {
		var eventIsGc bool
		select {
		case <-Clock.After(config.Resolution):
			// exit select.

		case <-gcTriggered:
			eventIsGc = true
			// exit select.

		case <-_watchdog.closing:
			// close and drain
			close(gcTriggered)
			for range gcTriggered {
			}
			gcTriggered = nil

			return
		}

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

		if eventIsGc {
			config.Logger.Infof("watchdog called after GC")
		}

		runtime.ReadMemStats(&memstats)

		if err := sysmem.Get(); err != nil {
			config.Logger.Warnf("failed to obtain system memory stats; err: %s", err)
		}

		trigger := config.Policy.Evaluate(PolicyInput{
			Scope:     config.Scope,
			Limit:     config.Limit,
			MemStats:  &memstats,
			SysStats:  &sysmem,
			GCTrigger: eventIsGc,
			Logger:    config.Logger,
		})

		if !trigger {
			continue
		}

		config.Logger.Infof("watchdog policy fired")

		if f := config.NotifyFired; f != nil {
			f()
		}

		if !config.NotifyOnly {
			config.Logger.Infof("watchdog is triggering GC")

			atomic.StoreInt32(&_watchdog.finalizerSkip, 1)

			// it's safe to assume that the finalizer will attempt to run before runtime.GC returns
			// because runtime.GC() waits for the sweep phase to finish before returning.
			// finalizers are run in the sweep phase.
			start := time.Now()
			runtime.GC()
			took := time.Since(start)

			atomic.StoreInt32(&_watchdog.finalizerSkip, 0)

			runtime.ReadMemStats(&memstats)
			config.Logger.Infof("watchdog-triggered GC finished; took: %s; current heap allocated: %d bytes", took, memstats.HeapAlloc)
		}
	}
}

func stopMemory() {
	if !atomic.CompareAndSwapInt32(&_watchdog.state, stateRunning, stateClosing) {
		return
	}

	close(_watchdog.closing)
	_watchdog.wg.Wait()

	atomic.StoreInt32(&_watchdog.state, stateUnstarted)
}
