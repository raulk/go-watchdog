# Go memory watchdog

> 🐺 A library to curb OOMs by running Go GC according to a user-defined policy.

[![godocs](https://img.shields.io/badge/godoc-reference-5272B4.svg?style=flat-square)](https://godoc.org/github.com/raulk/go-watchdog)
[![build status](https://circleci.com/gh/raulk/go-watchdog.svg?style=svg)](<LINK>)

go-watchdog runs a singleton memory watchdog in the process, which watches
memory utilization and forces Go GC in accordance with a user-defined policy.

There are two kinds of watchdog so far:

* **heap-driven:** applies a limit to the heap, and obtains current usage through
  `runtime.ReadMemStats()`.
* **system-driven:** applies a limit to the total system memory used, and obtains
  current usage through [`elastic/go-sigar`](https://github.com/elastic/gosigar).

A third process-driven watchdog that uses cgroups is underway.

This library ships with two policies out of the box:

* watermarks policy: runs GC at configured watermarks of system or heap memory
  utilisation.
* adaptive policy: runs GC when the current usage surpasses a dynamically-set
  threshold.
  
You can easily build a custom policy tailored to the allocation patterns of your
program.

## Why is this even needed?

The garbage collector that ships with the go runtime is pretty good in some
regards (low-latency, negligible no stop-the-world), but it's insatisfactory in
a number of situations that yield ill-fated outcomes:

1. it is incapable of dealing with bursty/spiky allocations efficiently;
   depending on the workload, the program may OOM as a consequence of not
   scheduling GC in a timely manner.
2. part of the above is due to the fact that go doesn't concern itself with any
   limits. To date, it is not possible to set a maximum heap size. 
2. its default policy of scheduling GC when the heap doubles, coupled with its
   ignorance of system or process limits, can easily cause it to OOM.

For more information, check out these GitHub issues:

* https://github.com/golang/go/issues/42805
* https://github.com/golang/go/issues/42430
* https://github.com/golang/go/issues/14735
* https://github.com/golang/go/issues/16843
* https://github.com/golang/go/issues/10064
* https://github.com/golang/go/issues/9849

## License

Dual-licensed: [MIT](./LICENSE-MIT), [Apache Software License v2](./LICENSE-APACHE), by way of the
[Permissive License Stack](https://protocol.ai/blog/announcing-the-permissive-license-stack/).
