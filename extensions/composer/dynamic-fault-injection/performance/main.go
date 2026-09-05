// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package main runs the process-level probability-distribution CPU benchmark.
package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/built-on-envoy/extensions/composer/dynamic-fault-injection/internal/fault"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "memory" {
		if len(os.Args) != 5 {
			panic("usage: performance memory stateful|stateless endpoint-count resolution")
		}
		runMemory(os.Args[2], os.Args[3], os.Args[4])
		return
	}
	if len(os.Args) != 2 {
		panic("usage: performance stateful|stateless")
	}
	dist, err := fault.NewResponseDistributionWithMode([]fault.StatusDistribution{{
		Status:       200,
		Resolution:   1000,
		Distribution: map[string]string{"p0.0": "1ms", "p50.0": "10ms", "p100.0": "100ms"},
	}, {
		Status:       503,
		Resolution:   100,
		Distribution: map[string]string{"p0.0": "1ms", "p50.0": "10ms", "p100.0": "100ms"},
	}}, os.Args[1])
	if err != nil {
		panic(err)
	}
	runtime.GOMAXPROCS(16)
	const workers = 16
	const duration = 3 * time.Second
	var ready sync.WaitGroup
	ready.Add(workers)
	start := make(chan struct{})
	counts := make([]uint64, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func(worker int) {
			defer wg.Done()
			ready.Done()
			<-start
			deadline := time.Now().Add(duration)
			var count uint64
			for {
				for i := 0; i < 1024; i++ {
					dist.Sample()
					count++
				}
				if time.Now().After(deadline) {
					break
				}
			}
			counts[worker] = count
		}(worker)
	}
	ready.Wait()
	started := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(started)
	var samples uint64
	for _, count := range counts {
		samples += count
	}
	fmt.Printf("mode=%s samples=%d elapsed=%s samples_per_second=%.0f\n", os.Args[1], samples, elapsed, float64(samples)/elapsed.Seconds())
}
