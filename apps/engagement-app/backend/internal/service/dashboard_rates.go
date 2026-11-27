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
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/cache"
)

// How long a resolved rate is remembered, and how long a failed lookup is
// remembered so a broken rate service is not called on every page load.
const (
	dashboardRatePastTTL    = 24 * time.Hour
	dashboardRateCurrentTTL = time.Hour
	dashboardRateFailedTTL  = 2 * time.Minute
	dashboardRateBudget     = 8 * time.Second
	dashboardRateParallel   = 8
	// A lookup runs past the request budget: at most this long for the call,
	// and at most this long waiting for a free slot.
	dashboardRateLookupTimeout = 30 * time.Second
	dashboardRateQueueTimeout  = 3 * time.Minute
)

// Where a USD amount came from.
const (
	rateSourceRecorded = "recorded"  // the original currency is USD
	rateSourceCard     = "card"      // the card was billed in USD, its own amount is used
	rateSourceMonthEnd = "month-end" // NetSuite's rate on the last day of the line's month
	rateSourceToday    = "today"     // NetSuite's rate today, for a line of the current month
)

// ratePair is one rate lookup: units of Currency for 1 USD on Date.
type ratePair struct{ Currency, Date string }

// rateSet holds the USD rates resolved for one request. A pair that could not
// be resolved is absent.
type rateSet map[ratePair]float64

// dashboardRateDate is the date whose NetSuite rate converts a line of month
// (YYYY-MM): the last day of the month when it is before today's month, else
// today (UTC).
func dashboardRateDate(month string, today time.Time) string {
	today = today.UTC()
	cur := today.Format("2006-01")
	t, err := time.Parse("2006-01", month)
	if err != nil || month >= cur {
		return today.Format(dashboardDateLayout)
	}
	return t.AddDate(0, 1, -1).Format(dashboardDateLayout)
}

// dashboardRates resolves and caches NetSuite's USD rate per (currency, rate
// date). One instance lives for the process; it is safe for concurrent use.
type dashboardRates struct {
	conv     FinanceCurrencyConverter
	now      func() time.Time
	budget   time.Duration
	past     *cache.Cache[string, float64]
	current  *cache.Cache[string, float64]
	failed   *cache.Cache[string, struct{}]
	mu       sync.Mutex
	inflight map[string]chan struct{}
	queued   map[string]chan struct{} // background lookups, closed when each ends
	sem      chan struct{}            // bounds lookups running at once
}

// newDashboardRates returns the rate cache over conv, which may be nil (then
// nothing resolves).
func newDashboardRates(conv FinanceCurrencyConverter, now func() time.Time) *dashboardRates {
	if now == nil {
		now = time.Now
	}
	return &dashboardRates{
		conv: conv, now: now, budget: dashboardRateBudget,
		past:     cache.New[string, float64](dashboardRatePastTTL),
		current:  cache.New[string, float64](dashboardRateCurrentTTL),
		failed:   cache.New[string, struct{}](dashboardRateFailedTTL),
		inflight: map[string]chan struct{}{},
		queued:   map[string]chan struct{}{},
		sem:      make(chan struct{}, dashboardRateParallel),
	}
}

func (r *dashboardRates) key(p ratePair) string { return p.Currency + "|" + p.Date }

// cacheFor is the cache for p: the short one for the current month.
func (r *dashboardRates) cacheFor(p ratePair) *cache.Cache[string, float64] {
	if len(p.Date) >= 7 && p.Date[:7] >= r.now().UTC().Format("2006-01") {
		return r.current
	}
	return r.past
}

// resolve returns the USD rate of every pair it can, and whether any pair is
// still being looked up (pending). Missing pairs are fetched in the
// background, at most dashboardRateParallel at a time across the process.
// resolve waits for them only for the budget; a lookup that finishes later
// still fills the cache, so the next call has it. Pairs the service failed or
// has no rate for are absent but not pending. Blank and USD currencies are
// skipped. Without a converter the result is empty.
func (r *dashboardRates) resolve(ctx context.Context, pairs []ratePair) (rateSet, bool) {
	out := rateSet{}
	if r == nil || r.conv == nil {
		return out, false
	}
	var todo []ratePair
	seen := map[ratePair]bool{}
	for _, p := range pairs {
		if p.Currency == "" || p.Currency == "USD" || seen[p] {
			continue
		}
		seen[p] = true
		if v, ok := r.cacheFor(p).Get(r.key(p)); ok {
			if v > 0 {
				out[p] = v
			}
			continue
		}
		if _, bad := r.failed.Get(r.key(p)); bad {
			continue
		}
		todo = append(todo, p)
	}
	if len(todo) == 0 {
		return out, false
	}

	start := time.Now()
	waits := make([]chan struct{}, len(todo))
	for i, p := range todo {
		waits[i] = r.start(ctx, p)
	}
	bctx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
wait:
	for _, ch := range waits {
		select {
		case <-ch:
		case <-bctx.Done():
			break wait
		}
	}

	pending := false
	for _, p := range todo {
		if v, ok := r.cacheFor(p).Get(r.key(p)); ok {
			if v > 0 {
				out[p] = v
			}
		} else if _, bad := r.failed.Get(r.key(p)); !bad {
			pending = true
		}
	}
	slog.InfoContext(ctx, "dashboard rates resolved", "requested", len(todo), "resolved", len(out), "pending", pending, "duration", time.Since(start).String())
	return out, pending
}

// isPending is whether p has neither an answer nor a failure yet, so a lookup is still running.
func (r *dashboardRates) isPending(p ratePair) bool {
	if r == nil || r.conv == nil || p.Currency == "" || p.Currency == "USD" {
		return false
	}
	if _, ok := r.cacheFor(p).Get(r.key(p)); ok {
		return false
	}
	_, bad := r.failed.Get(r.key(p))
	return !bad
}

// start makes sure p is being looked up and returns a channel that closes when
// that lookup ends. A lookup already running for p is joined. The lookup is
// detached from ctx, so it outlives the request that asked and still fills the
// cache; it is bounded by dashboardRateQueueTimeout (waiting for a slot) and
// dashboardRateLookupTimeout (the call itself).
func (r *dashboardRates) start(ctx context.Context, p ratePair) chan struct{} {
	k := r.key(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.queued[k]; ok {
		return ch
	}
	ch := make(chan struct{})
	r.queued[k] = ch
	lctx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(lctx, "dashboard rate lookup panicked", "currency", p.Currency, "date", p.Date, "panic", rec)
			}
			r.mu.Lock()
			delete(r.queued, k)
			r.mu.Unlock()
			close(ch)
		}()
		qctx, qcancel := context.WithTimeout(lctx, dashboardRateQueueTimeout)
		defer qcancel()
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-qctx.Done():
			return
		}
		fctx, fcancel := context.WithTimeout(lctx, dashboardRateLookupTimeout)
		defer fcancel()
		r.fetch(fctx, p)
	}()
	return ch
}

// fetch gets one rate, sharing the call with any other request that asks for
// the same pair at the same time.
func (r *dashboardRates) fetch(ctx context.Context, p ratePair) (float64, bool) {
	k := r.key(p)
	r.mu.Lock()
	// Another request may have finished this pair between resolve's check and
	// this lock, so look again before registering as the one that calls out.
	if v, ok := r.cacheFor(p).Get(k); ok {
		r.mu.Unlock()
		return v, v > 0
	}
	if _, bad := r.failed.Get(k); bad {
		r.mu.Unlock()
		return 0, false
	}
	if ch, busy := r.inflight[k]; busy {
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, false
		}
		v, ok := r.cacheFor(p).Get(k)
		return v, ok && v > 0
	}
	ch := make(chan struct{})
	r.inflight[k] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, k)
		r.mu.Unlock()
		close(ch)
	}()

	rates, err := r.conv.Rates(ctx, p.Currency, p.Date)
	if err != nil {
		// A cancelled request or a spent budget says nothing about the rate
		// service, so only an answer from the service itself is remembered.
		if ctx.Err() != nil {
			return 0, false
		}
		slog.WarnContext(ctx, "dashboard rate lookup failed", "currency", p.Currency, "date", p.Date, "err", err)
		r.failed.Set(k, struct{}{})
		return 0, false
	}
	v, ok := rates["USD"]
	if !ok || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		// The service answered and has no usable USD rate for this pair. That
		// is a settled answer, so it is kept as long as a rate would be
		// (stored as 0) instead of being asked again every two minutes.
		r.cacheFor(p).Set(k, 0)
		return 0, false
	}
	r.cacheFor(p).Set(k, v)
	return v, true
}

// dashboardUSD is one amount in USD with how it was got. USD is already
// rounded to cents (see usd), so sums of it are sums of cents.
type dashboardUSD struct {
	USD      float64
	Rate     float64 // units of the original currency for 1 USD; 1 when recorded
	RateDate string  // blank when no rate was used
	Source   string
}

// usdNeedsRate reports whether a line must be converted with a NetSuite rate:
// its currency is not USD and the card was not billed in USD.
func usdNeedsRate(currency, ccCurrency string) bool {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	return currency != "" && currency != "USD" && strings.ToUpper(strings.TrimSpace(ccCurrency)) != "USD"
}

// usd converts one amount per the rule: a USD original is used as it is; a
// card billed in USD uses the billed amount; anything else is divided by the
// rate of its currency on the rate date of its month. ok is false when the
// amount cannot be converted (blank currency, no rate).
func (rs rateSet) usd(currency string, amount float64, ccCurrency string, ccAmount float64, month string, today time.Time) (dashboardUSD, bool) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	switch {
	case currency == "USD":
		return dashboardUSD{USD: dashboardRound(amount), Rate: 1, Source: rateSourceRecorded}, true
	case strings.ToUpper(strings.TrimSpace(ccCurrency)) == "USD":
		return dashboardUSD{USD: dashboardRound(ccAmount), Rate: 1, Source: rateSourceCard}, true
	case currency == "":
		return dashboardUSD{}, false
	}
	date := dashboardRateDate(month, today)
	rate, ok := rs[ratePair{currency, date}]
	if !ok || rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return dashboardUSD{}, false
	}
	// Round each line to cents once, here, so every total (KPIs, breakdowns,
	// transactions header, CSVs) is the sum of the cent amounts shown on the
	// lines and never differs from them by accumulated rounding.
	v := dashboardRound(amount / rate)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return dashboardUSD{}, false
	}
	src := rateSourceMonthEnd
	if date == today.UTC().Format(dashboardDateLayout) {
		src = rateSourceToday
	}
	return dashboardUSD{USD: v, Rate: rate, RateDate: date, Source: src}, true
}
