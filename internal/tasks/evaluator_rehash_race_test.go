// Package tasks provides module-level functionality for tasks.
// input: chainFacts iterating a task's rehash inner map while setRehash writes the same map under e.mu
// output: no concurrent map read/write (run with -race); frequent rotation must not crash the node
// pos: #413 regression
// note: if this file changes, update this header and module README.md.
package tasks

// #413: chainFacts read e.rehash[task.ID] (the per-task inner map) after
// releasing e.mu while setRehash writes that same map under e.mu, so frequent
// rotation crashed a node with "concurrent map read and map write". Run with
// -race: the detector reports the unsynchronised read even when the runtime
// check does not fire.

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChainFacts_RehashMapNoRaceWithSetRehash(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	var names []string
	for i, seqs := range [][]int64{{37, 38}, {39, 40}, {41, 42}} {
		name := fmt.Sprintf("mysql-bin.%06d", i+1)
		writeAlertSegment(t, filepath.Join(dir, name), seqs...)
		names = append(names, name)
	}
	e := newEval(t, s, nil)
	// a mismatch already recorded, so chainFacts iterates the inner map
	e.setRehash(task.ID, names[0], rehashResult{ok: false, evidence: "seed"})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the rehasher: keeps writing the same task's inner map
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.setRehash(task.ID, names[i%len(names)], rehashResult{ok: i%2 == 0, evidence: "w"})
			e.setRehash(task.ID, fmt.Sprintf("mysql-bin.%06d", 100+i%50), rehashResult{ok: true})
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.chainFacts(task, time.Now())
	}
	close(stop)
	wg.Wait()
}
