package watchdog

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/containerd/cgroups"
)

var (
	pid          = os.Getpid()
	memSubsystem = cgroups.SingleSubsystem(cgroups.V1, cgroups.Memory)
)

// OnMemoryPressure uses cgroups memory pressure notifications to trigger GC.
//
// EXPERIMENTAL. Do not use. This implementation has two problems:
//
// 1. Container runtimes mount /sys/fs/cgroup/memory as read-only, which
//    prevents us from writing to cgroup.event_control to request notifications.
// 2. The stopFn does not really shut down the watchdog because I haven't found
//    a way to unblock the read. This likely doesn't matter much because the
//    watchdog
//
// It uses a "medium,hierarchy" strategy, which means:
//
// * medium: notification threshold; we will get notified when the system is
//   actively reclaiming memory pages, and the ratio of scanned vs. reclaimed
//   exceeds 60%; that is, 60% of scanned pages weren't reclaimable.
// * hierarchy: propagates the event up through the control group hierarchy.
//   (the default behaviour is to swallow it at the first notifee).
//
// For more info, refer to the kernel docs at
// https://www.kernel.org/doc/Documentation/cgroup-v1/memory.txt, and to the
// patch that introduced this feature https://lwn.net/Articles/544652/.
func OnMemoryPressure() (err error, stopFn func()) {
	// use self path unless our PID is 1, in which case we're running inside
	// a container and our limits are in the root path.
	path := cgroups.NestedPath("")
	if pid := os.Getpid(); pid == 1 {
		path = cgroups.RootPath
	}

	cgroup, err := cgroups.Load(memSubsystem, path)
	if err != nil {
		return fmt.Errorf("failed to load cgroup for process: %w", err), nil
	}

	stat, err := cgroup.Stat()
	fmt.Println(stat.Memory.Usage.Limit)
	fmt.Println(stat.Memory.Usage.Usage)

	evt := cgroups.MemoryPressureEvent(cgroups.MediumPressure, cgroups.HierarchyMode)
	// evt := cgroups.MemoryThresholdEvent(20000000, true)
	fd, err := cgroup.RegisterMemoryEvent(evt)
	if err != nil {
		return fmt.Errorf("failed to register for memory events: %w", err), nil
	}

	if err := start(UtilizationProcess); err != nil {
		return err, nil
	}

	eventFile := os.NewFile(fd, "eventfd")
	localClose := make(chan struct{})

	_watchdog.wg.Add(1)
	go func() {
		select {
		case <-_watchdog.closing:
		case <-localClose:
		}
		_ = syscall.Close(int(eventFile.Fd()))
	}()

	_watchdog.wg.Add(1)
	go func() {
		defer _watchdog.wg.Done()

		signal := make([]byte, 8)
		var memstats runtime.MemStats

		for {
			if _, err := eventFile.Read(signal); err != nil {
				fmt.Println(err)
				close(localClose)
				return
			}

			fmt.Printf("triggered: %x\n", signal)
			// check if the event group is destroyed; if so, we would've
			// received an event, but it's not an actual memory pressure event.
			if cgroup.State() == cgroups.Deleted {
				close(localClose)
				return
			}

			runtime.ReadMemStats(&memstats)
			Logger.Infof("memory-pressure watchdog triggered; current heap usage: %d bytes", memstats.HeapAlloc)

			// triggering GC.
			forceGC(&memstats)
		}
	}()

	return nil, stop
}
