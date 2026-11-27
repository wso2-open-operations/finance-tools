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
	"sync"
	"testing"
	"time"
)

func TestDashboardRateDate(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		month string
		today time.Time
		want  string
	}{
		{"2026-10", at("2026-11-05 09:00"), "2026-10-31"},
		{"2026-09", at("2026-11-05 09:00"), "2026-09-30"},
		{"2028-02", at("2028-05-01 00:00"), "2028-02-29"}, // leap year
		{"2027-02", at("2027-05-01 00:00"), "2027-02-28"},
		{"2026-11", at("2026-11-05 09:00"), "2026-11-05"}, // current month: today
		{"2026-10", at("2026-11-30 23:59"), "2026-10-31"}, // last day of the current month, 23:59 UTC
		{"2026-11", at("2026-11-30 23:59"), "2026-11-30"},
		{"2026-12", at("2026-12-01 00:00"), "2026-12-01"},
	}
	for _, c := range cases {
		if got := dashboardRateDate(c.month, c.today); got != c.want {
			t.Errorf("dashboardRateDate(%s, %s) = %s, want %s", c.month, c.today.Format(time.RFC3339), got, c.want)
		}
	}
}

func TestDashboardRates_CachesAcrossResolves(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300, "EUR": 0.9}}
	r := newDashboardRates(f, dashboardNow)
	pairs := []ratePair{{"LKR", "2026-10-31"}, {"EUR", "2026-10-31"}, {"LKR", "2026-10-31"}, {"USD", "2026-10-31"}, {"", "2026-10-31"}}
	got, _ := r.resolve(context.Background(), pairs)
	if len(got) != 2 || got[ratePair{"LKR", "2026-10-31"}] != 300 || f.count() != 2 {
		t.Fatalf("first resolve = %v after %d calls", got, f.count())
	}
	got, _ = r.resolve(context.Background(), pairs)
	if len(got) != 2 || f.count() != 2 {
		t.Errorf("second resolve = %v after %d calls, want no new call", got, f.count())
	}
}

func TestDashboardRates_FailureIsRemembered(t *testing.T) {
	f := &fakeRates{err: errors.New("down")}
	r := newDashboardRates(f, dashboardNow)
	p := []ratePair{{"LKR", "2026-10-31"}}
	if got, _ := r.resolve(context.Background(), p); len(got) != 0 {
		t.Fatalf("resolve = %v", got)
	}
	_, _ = r.resolve(context.Background(), p)
	if f.count() != 1 {
		t.Errorf("calls = %d, want 1 (the failure is remembered for 2 minutes)", f.count())
	}
}

func TestDashboardRates_BadRatesAreUnresolved(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 0, "EUR": -2, "GBP": 0}}
	r := newDashboardRates(f, dashboardNow)
	got, _ := r.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}, {"EUR", "2026-10-31"}, {"JPY", "2026-10-31"}})
	if len(got) != 0 {
		t.Errorf("resolve = %v, want none", got)
	}
}

func TestDashboardRates_BudgetLeavesSlowPairsUnresolved(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300}, delay: 5 * time.Second}
	r := newDashboardRates(f, dashboardNow)
	r.budget = 50 * time.Millisecond
	start := time.Now()
	got, _ := r.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}})
	if len(got) != 0 || time.Since(start) > 2*time.Second {
		t.Errorf("resolve = %v after %v, want empty and quick", got, time.Since(start))
	}
}

func TestDashboardRates_SlowLookupFillsCachePastBudget(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300}, delay: 150 * time.Millisecond}
	r := newDashboardRates(f, dashboardNow)
	r.budget = 20 * time.Millisecond
	p := []ratePair{{"LKR", "2026-10-31"}}
	got, pending := r.resolve(context.Background(), p)
	if len(got) != 0 || !pending {
		t.Fatalf("resolve = %v pending=%v, want none and pending", got, pending)
	}
	if !r.isPending(p[0]) {
		t.Error("a pair still being looked up is pending")
	}
	time.Sleep(400 * time.Millisecond)
	if r.isPending(p[0]) {
		t.Error("a settled pair is not pending")
	}
	got, pending = r.resolve(context.Background(), p)
	if got[p[0]] != 300 || pending || f.count() != 1 {
		t.Errorf("resolve = %v pending=%v after %d calls, want 300 from cache, one call", got, pending, f.count())
	}
}

func TestDashboardRates_SettledAnswersAreNotPending(t *testing.T) {
	f := &fakeRates{err: errors.New("down")}
	r := newDashboardRates(f, dashboardNow)
	if _, pending := r.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}}); pending {
		t.Error("a failed lookup is not pending")
	}
	f2 := &fakeRates{rates: map[string]float64{"LKR": 0}}
	r2 := newDashboardRates(f2, dashboardNow)
	if _, pending := r2.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}}); pending {
		t.Error("an answer with no USD rate is not pending")
	}
}

func TestDashboardRates_JoinsLookupInProgress(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300}, delay: 100 * time.Millisecond}
	r := newDashboardRates(f, dashboardNow)
	r.budget = 10 * time.Millisecond
	p := []ratePair{{"LKR", "2026-10-31"}}
	r.resolve(context.Background(), p)
	r.budget = time.Second
	got, pending := r.resolve(context.Background(), p)
	if got[p[0]] != 300 || pending || f.count() != 1 {
		t.Errorf("resolve = %v pending=%v after %d calls, want the one lookup shared", got, pending, f.count())
	}
}

func TestDashboardRates_NilConverter(t *testing.T) {
	r := newDashboardRates(nil, dashboardNow)
	if got, _ := r.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}}); len(got) != 0 {
		t.Errorf("resolve = %v", got)
	}
}

func TestDashboardRates_ConcurrentRequestsShareACall(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300}, delay: 50 * time.Millisecond}
	r := newDashboardRates(f, dashboardNow)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _ := r.resolve(context.Background(), []ratePair{{"LKR", "2026-10-31"}}); got[ratePair{"LKR", "2026-10-31"}] != 300 {
				t.Errorf("resolve = %v", got)
			}
		}()
	}
	wg.Wait()
	if f.count() != 1 {
		t.Errorf("calls = %d, want 1", f.count())
	}
}

func TestRateSetUSD(t *testing.T) {
	today := dashboardNow()
	rs := rateSet{{"LKR", "2026-10-31"}: 300, {"LKR", "2026-11-05"}: 310}
	u, ok := rs.usd("usd", 12.5, "", 0, "2026-10", today)
	if !ok || u.USD != 12.5 || u.Rate != 1 || u.Source != rateSourceRecorded || u.RateDate != "" {
		t.Errorf("usd original = %+v %v", u, ok)
	}
	u, ok = rs.usd("LKR", 15000, "USD", 48.2, "2026-10", today)
	if !ok || u.USD != 48.2 || u.Source != rateSourceCard {
		t.Errorf("card billed in USD = %+v %v", u, ok)
	}
	u, ok = rs.usd("LKR", 90000, "", 0, "2026-10", today)
	if !ok || u.USD != 300 || u.Rate != 300 || u.RateDate != "2026-10-31" || u.Source != rateSourceMonthEnd {
		t.Errorf("month end = %+v %v", u, ok)
	}
	u, ok = rs.usd("LKR", 3100, "", 0, "2026-11", today)
	if !ok || u.USD != 10 || u.RateDate != "2026-11-05" || u.Source != rateSourceToday {
		t.Errorf("current month = %+v %v", u, ok)
	}
	if _, ok = rs.usd("EUR", 1, "", 0, "2026-10", today); ok {
		t.Error("a currency without a rate converted")
	}
	if _, ok = rs.usd(" ", 1, "", 0, "2026-10", today); ok {
		t.Error("a blank currency converted")
	}
}

func TestDashboardRates_AnswerWithNoUsableRateIsKeptAsLongAsARate(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 0}}
	r := newDashboardRates(f, dashboardNow)
	p := ratePair{"LKR", "2026-10-31"}
	if got, _ := r.resolve(context.Background(), []ratePair{p}); len(got) != 0 {
		t.Fatalf("resolve = %v, want none", got)
	}
	_, _ = r.resolve(context.Background(), []ratePair{p})
	if f.count() != 1 {
		t.Errorf("calls = %d, want 1", f.count())
	}
	if _, short := r.failed.Get(r.key(p)); short {
		t.Error("a settled answer must not sit in the 2 minute failure cache")
	}
	if _, kept := r.past.Get(r.key(p)); !kept {
		t.Error("a settled answer must be kept in the rate cache for its normal TTL")
	}
}

func TestDashboardRates_FetchLooksAgainBeforeCallingOut(t *testing.T) {
	f := &fakeRates{rates: map[string]float64{"LKR": 300}}
	r := newDashboardRates(f, dashboardNow)
	p := ratePair{"LKR", "2026-10-31"}
	// A request that finished between resolve's check and fetch's lock leaves the pair cached.
	r.past.Set(r.key(p), 310)
	if v, ok := r.fetch(context.Background(), p); !ok || v != 310 || f.count() != 0 {
		t.Errorf("fetch = %v, %v after %d calls, want the cached 310 and no call", v, ok, f.count())
	}
}
