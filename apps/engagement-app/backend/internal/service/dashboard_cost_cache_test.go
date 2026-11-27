// Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A caller that joins a load in flight stops waiting when its own request ends, and the load goes on for the others.
func TestCostSetCache_WaiterStopsWhenItsContextEnds(t *testing.T) {
	c := newCostSetCache(4, time.Minute, nil)
	release := make(chan struct{})
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.get(context.Background(), "k", func() (*costSet, error) {
			close(started)
			<-release
			return &costSet{}, nil
		})
		leaderDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "k", func() (*costSet, error) { t.Error("a waiter must not load"); return nil, nil })
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if err != context.Canceled {
			t.Errorf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter kept waiting after its context ended")
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Errorf("leader error = %v", err)
	}
}

// Different keys load at most costSetMaxLoads at once; one more waits for a
// slot, gives up when its request ends, and loads once a slot frees.
func TestCostSetCache_BoundsConcurrentLoads(t *testing.T) {
	c := newCostSetCache(8, time.Minute, nil)
	release := make(chan struct{})
	started := make(chan struct{}, costSetMaxLoads)
	done := make(chan struct{}, costSetMaxLoads)
	for i := 0; i < costSetMaxLoads; i++ {
		key := string(rune('a' + i))
		go func() {
			_, _ = c.get(context.Background(), key, func() (*costSet, error) {
				started <- struct{}{}
				<-release
				return &costSet{}, nil
			})
			done <- struct{}{}
		}()
	}
	for i := 0; i < costSetMaxLoads; i++ {
		<-started
	}
	ran := false
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.get(ctx, "extra", func() (*costSet, error) { ran = true; return &costSet{}, nil }); !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("over the bound: err = %v, ran = %v; want a deadline error and no load", err, ran)
	}
	close(release)
	for i := 0; i < costSetMaxLoads; i++ {
		<-done
	}
	if _, err := c.get(context.Background(), "extra", func() (*costSet, error) { ran = true; return &costSet{}, nil }); err != nil || !ran {
		t.Fatalf("after a slot frees: err = %v, ran = %v", err, ran)
	}
}

// A joiner whose own request is live starts over when the caller that queued
// the key leaves before a slot opens.
func TestCostSetCache_JoinerRetriesWhenTheQueuedCallerLeaves(t *testing.T) {
	c := newCostSetCache(8, time.Minute, nil)
	release := make(chan struct{})
	started := make(chan struct{}, costSetMaxLoads)
	for i := 0; i < costSetMaxLoads; i++ {
		key := string(rune('a' + i))
		go func() {
			_, _ = c.get(context.Background(), key, func() (*costSet, error) {
				started <- struct{}{}
				<-release
				return &costSet{}, nil
			})
		}()
	}
	for i := 0; i < costSetMaxLoads; i++ {
		<-started
	}
	leaderCtx, leave := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.get(leaderCtx, "extra", func() (*costSet, error) { return &costSet{}, nil })
		leaderDone <- err
	}()
	for { // wait until the leader has queued the key
		c.mu.Lock()
		_, queued := c.inflight["extra"]
		c.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	joinerDone := make(chan error, 1)
	go func() {
		_, err := c.get(context.Background(), "extra", func() (*costSet, error) { return &costSet{}, nil })
		joinerDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	leave()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	close(release)
	select {
	case err := <-joinerDone:
		if err != nil {
			t.Fatalf("joiner err = %v, want it to load on its own context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("joiner never finished")
	}
}

// A get after a clear starts its own load instead of joining one that began before the clear, and the old load finishing later does not drop the new one.
func TestLoadCache_ClearStopsLaterGetsJoiningOlderLoads(t *testing.T) {
	c := newLoadCache[string](4, time.Hour, 2, nil)
	started, release := make(chan struct{}), make(chan struct{})
	oldDone := make(chan string, 1)
	go func() {
		v, _ := c.get(context.Background(), "k", func() (string, error) {
			close(started)
			<-release
			return "before", nil
		})
		oldDone <- v
	}()
	<-started
	c.clear()

	got, err := c.get(context.Background(), "k", func() (string, error) { return "after", nil })
	if err != nil || got != "after" {
		t.Fatalf("get after clear = %q, %v; want the load that began after the clear", got, err)
	}
	close(release)
	if v := <-oldDone; v != "before" {
		t.Fatalf("older caller = %q, want its own result", v)
	}
	got, _ = c.get(context.Background(), "k", func() (string, error) { return "reloaded", nil })
	if got != "after" {
		t.Fatalf("cached value = %q, want %q (older load must not be kept)", got, "after")
	}
}

func TestCostSetCache_RatesPendingSetIsKeptBriefly(t *testing.T) {
	clock := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	c := newCostSetCache(4, time.Hour, func() time.Time { return clock })
	ctx := context.Background()
	loads := 0
	load := func(set *costSet) func() (*costSet, error) {
		return func() (*costSet, error) { loads++; return set, nil }
	}
	_, _ = c.get(ctx, "pending", load(&costSet{ratesPending: true}))
	clock = clock.Add(30 * time.Second)
	_, _ = c.get(ctx, "pending", load(&costSet{}))
	if loads != 1 {
		t.Fatalf("loads = %d, want 1 (still kept after 30s)", loads)
	}
	clock = clock.Add(2 * time.Minute)
	_, _ = c.get(ctx, "pending", load(&costSet{}))
	if loads != 2 {
		t.Errorf("loads = %d, want 2 (reloaded after the incomplete TTL)", loads)
	}
}

func TestCostPrepared_IncompleteTextNamesPendingRates(t *testing.T) {
	c := &costPrepared{set: &costSet{ratesPending: true}}
	if got := c.incompleteText(); got != "Exchange rates still loading" {
		t.Errorf("incompleteText = %q", got)
	}
}
