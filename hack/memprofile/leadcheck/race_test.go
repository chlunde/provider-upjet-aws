// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"sync"
	"testing"
	"time"
)

// The code below is a transcription of
// internal/clients/cache.go:96-149 (CallerIdentityCache.GetCallerIdentity and
// makeRoom) with the AWS types replaced by an int payload, so that the lock
// discipline can be exercised under `go test -race` without linking
// xpprovider. Nothing else about it is changed: the entry pointer is read
// under RLock, the RLock is released, and AccessedAt is then read outside any
// lock while a second goroutine writes it under the write lock.
//
// Run: go test -race ./hack/memprofile/leadcheck/ -run TestCallerIdentityCacheRace

type entry struct {
	payload    int
	AccessedAt time.Time
}

type cache struct {
	cache   map[string]*entry
	maxSize int
	mu      *sync.RWMutex
	misses  int
}

func (c *cache) get(key string) int {
	c.mu.RLock()
	o, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		// cache.go:111-115 — unsynchronised read of o.AccessedAt.
		if time.Since(o.AccessedAt) > 10*time.Minute {
			c.mu.Lock()
			o.AccessedAt = time.Now() // cache.go:113 — write under the lock.
			c.mu.Unlock()
		}
		return o.payload
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.misses++
	c.cache[key] = &entry{AccessedAt: time.Now(), payload: c.misses}
	return c.misses
}

func TestCallerIdentityCacheRace(t *testing.T) {
	// This test is expected to FAIL under -race: that is its whole point. It is
	// skipped unless explicitly requested so that `go test ./...` stays green.
	if os.Getenv("LEADCHECK_RACE") == "" {
		t.Skip("set LEADCHECK_RACE=1 and run with -race to reproduce the data race described in docs/lead-triage.md (L8)")
	}
	c := &cache{cache: map[string]*entry{}, maxSize: 100, mu: &sync.RWMutex{}}
	// Warm the single shared entry, then age it past the 10 minute threshold so
	// that every subsequent caller takes the write branch — this is the steady
	// state ten minutes after any provider start-up.
	c.get("k")
	c.cache["k"].AccessedAt = time.Now().Add(-time.Hour)

	// The ager stands in for the passage of wall-clock time, which is what
	// re-arms the > 10 minute branch in production. It writes AccessedAt under
	// the write lock, exactly as cache.go:112-114 does; the racing partner the
	// detector reports is the *unlocked* read at get()'s `time.Since`.
	done := make(chan struct{})
	var ager sync.WaitGroup
	ager.Add(1)
	go func() {
		defer ager.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			c.mu.Lock()
			c.cache["k"].AccessedAt = time.Now().Add(-time.Hour)
			c.mu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				c.get("k")
			}
		}()
	}
	wg.Wait()
	close(done)
	ager.Wait()
}
