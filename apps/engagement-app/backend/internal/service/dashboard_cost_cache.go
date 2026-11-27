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
	"sync"
	"time"
)

// Dashboard data is cached in this pod and shared by every caller. What is
// cached is only data that is identical for everyone for the same query (cost
// line sets per date range, entity aggregates per filter); each caller's
// privilege filtering and scope are applied after the read, on every request.
// Personal work queues and anything "mine" are never cached.
const (
	// costSetCacheSize and costSetMaxLines bound the cost sets one pod keeps.
	// A set holds every line of a date range (up to about 80,000 lines, a few
	// hundred bytes each), so the budget is by lines: 200,000 lines is roughly
	// 100 MB at worst. The least recently used sets go first, and a set larger
	// than the budget is served but not kept.
	costSetCacheSize = 16
	costSetMaxLines  = 200_000
	// costSetMaxLoads bounds how many different cost sets one pod reads at
	// once, whatever the output format; each holds up to a whole year of
	// lines. A request over the bound waits for a slot until its ctx ends.
	costSetMaxLoads = 3
	// dashboardAggCacheSize bounds the entity aggregates (small, a few KB each).
	dashboardAggCacheSize = 256
	dashboardAggMaxLoads  = 8
	// cacheIncompleteTTL is how long a result that is partial (a failed source,
	// truncated lines or exchange rates still loading) is kept: briefly, so the gap is retried soon.
	cacheIncompleteTTL = time.Minute
)

// loadCache is an LRU with a TTL and a line/weight budget. It shares one load
// between requests that ask for the same key at the same time (singleflight),
// bounds how many different keys load at once, never keeps a failed load, and
// can be cleared when data changes. cache.Cache has no size bound, so it is
// not used here.
type loadCache[V any] struct {
	mu        sync.Mutex
	size      int
	ttl       time.Duration // zero or negative: nothing is kept
	maxWeight int           // zero: no weight budget
	weight    func(V) int
	// ttlFor shortens the TTL of one value (nil: ttl for all).
	ttlFor   func(V) time.Duration
	now      func() time.Time
	items    map[string]*cacheEntry[V]
	order    []string // least recently used first
	total    int      // weight of the items
	gen      uint64   // bumped by clear; a load that started before it is not kept
	inflight map[string]*cacheCall[V]
	loads    chan struct{}
}

type cacheEntry[V any] struct {
	val     V
	weight  int
	expires time.Time
}

type cacheCall[V any] struct {
	done      chan struct{}
	val       V
	err       error
	abandoned bool // the caller that queued it left before a slot opened; set before done closes
}

// costSetCache holds the cost line sets.
type costSetCache = loadCache[*costSet]

func newCostSetCache(size int, ttl time.Duration, now func() time.Time) *costSetCache {
	c := newLoadCache[*costSet](size, ttl, costSetMaxLoads, now)
	c.maxWeight = costSetMaxLines
	c.weight = func(s *costSet) int { return len(s.lines) }
	c.ttlFor = func(s *costSet) time.Duration {
		if s.truncated || len(s.failed) > 0 || s.ratesPending {
			return min(ttl, cacheIncompleteTTL)
		}
		return ttl
	}
	return c
}

func newLoadCache[V any](size int, ttl time.Duration, maxLoads int, now func() time.Time) *loadCache[V] {
	if now == nil {
		now = time.Now
	}
	return &loadCache[V]{
		size: size, ttl: ttl, now: now,
		items: map[string]*cacheEntry[V]{}, inflight: map[string]*cacheCall[V]{},
		loads: make(chan struct{}, maxLoads),
	}
}

func (c *loadCache[V]) touch(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
}

func (c *loadCache[V]) evict(key string) {
	if e, ok := c.items[key]; ok {
		c.total -= e.weight
		delete(c.items, key)
	}
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// clear drops everything kept and makes loads already running not keep their
// result, so data read before a write is never kept after it. Later gets start
// their own load instead of joining one that began before the clear.
func (c *loadCache[V]) clear() {
	c.mu.Lock()
	c.items = map[string]*cacheEntry[V]{}
	c.order, c.total = nil, 0
	c.gen++
	c.inflight = map[string]*cacheCall[V]{}
	c.mu.Unlock()
}

// acquire waits for one of the read slots until ctx ends. Every read from the
// entity takes one, and gives it back with release.
func (c *loadCache[V]) acquire(ctx context.Context) error {
	select {
	case c.loads <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *loadCache[V]) release() { <-c.loads }

// get returns the cached value for key, or runs load once for all callers that
// ask at the same time and caches its result (a failure is not cached). A
// caller that joins a load in flight stops waiting when its own ctx ends;
// the load goes on for the others. Only the cache's load bound of different
// keys load at once; a new key waits for a slot while its ctx lasts. If the
// caller that queued the key gives up before it gets a slot, the callers
// that joined it try again on their own contexts.
func (c *loadCache[V]) get(ctx context.Context, key string, load func() (V, error)) (V, error) {
	var zero V
	for {
		c.mu.Lock()
		if e, ok := c.items[key]; ok {
			if c.now().Before(e.expires) {
				c.touch(key)
				c.mu.Unlock()
				return e.val, nil
			}
			c.evict(key)
		}
		if call, busy := c.inflight[key]; busy {
			c.mu.Unlock()
			select {
			case <-call.done:
				if call.abandoned {
					continue
				}
				return call.val, call.err
			case <-ctx.Done():
				return zero, ctx.Err()
			}
		}
		call := &cacheCall[V]{done: make(chan struct{})}
		c.inflight[key] = call
		gen := c.gen
		c.mu.Unlock()

		if err := c.acquire(ctx); err != nil {
			// No slot in time: nothing is cached and joiners start over.
			call.abandoned = true
			c.mu.Lock()
			if c.inflight[key] == call {
				delete(c.inflight, key)
			}
			c.mu.Unlock()
			close(call.done)
			return zero, err
		}
		call.val, call.err = load()
		c.release()

		c.mu.Lock()
		if c.inflight[key] == call {
			delete(c.inflight, key)
		}
		if call.err == nil {
			c.store(key, call.val, gen)
		}
		c.mu.Unlock()
		close(call.done)
		return call.val, call.err
	}
}

// store keeps v unless caching is off, a clear happened since the load began,
// or v alone is over the weight budget. The caller holds mu.
func (c *loadCache[V]) store(key string, v V, gen uint64) {
	ttl := c.ttl
	if c.ttlFor != nil {
		ttl = c.ttlFor(v)
	}
	w := 0
	if c.weight != nil {
		w = c.weight(v)
	}
	if ttl <= 0 || gen != c.gen || (c.maxWeight > 0 && w > c.maxWeight) {
		return
	}
	c.evict(key)
	c.items[key] = &cacheEntry[V]{val: v, weight: w, expires: c.now().Add(ttl)}
	c.total += w
	c.touch(key)
	for len(c.order) > 1 && (len(c.order) > c.size || (c.maxWeight > 0 && c.total > c.maxWeight)) {
		c.evict(c.order[0])
	}
}
