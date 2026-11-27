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
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/clients/engagemententity"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/domain"
)

func finPerms(own ...int) domain.Perms {
	t := append(append([]int{}, dashboardEmployeeTravel...), own...)
	return dashboardPerms("fin@wso2.com", dashboardEmployeeAlloc, dashboardEmployeeAlloc, t, t, "App-Travel-Approver-Finance-stg")
}

func tsaPerms() domain.Perms {
	return dashboardPerms("tsa@wso2.com", dashboardEmployeeAlloc, dashboardEmployeeAlloc, dashboardRange(1, 49), dashboardEmployeeTravel, "app-travel-admin-super-stg")
}

func entry(id, source, job, date, cur string, amt float64) engagemententity.DashboardCostEntry {
	e := engagemententity.DashboardCostEntry{ID: id, Source: source, Date: date, OriginalCurrency: cur, OriginalAmount: amt, Paid: "unknown", CostType: "Other"}
	switch source {
	case "perDiem":
		e.CostType = "Per Diem"
	case "card":
		e.CostType = "Card"
	case "claim":
		e.CostType = "Claim"
	}
	if job != "" {
		e.JobNumber = sptr(job)
	}
	return e
}

// costService serves lines (the same for any range unless fn is set).
func costService(lines []engagemententity.DashboardCostEntry, conv FinanceCurrencyConverter) (DashboardService, *fakeDashboardEntity) {
	f := &fakeDashboardEntity{costResp: &engagemententity.DashboardCostLines{Items: lines}}
	return NewDashboardService(f, nil, conv, dashboardNow), f
}

var cq = CostQuery{From: "2026-01-01", To: "2026-11-05", Compare: true}

func mustSummary(t *testing.T, svc DashboardService, p domain.Perms, q CostQuery) *domain.TravelCostSummary {
	t.Helper()
	s, err := svc.TravelCosts(context.Background(), p, q)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTravelCosts_KpisBucketsAndTotals(t *testing.T) {
	a := entry("INV-1", "invoice", "TRV-1", "2026-10-14", "LKR", 90000)
	a.Country, a.Customer, a.Paid, a.TravelAgentID, a.TravelAgentName, a.TravellerEmail = sptr("India"), sptr("Acme"), "paid", iptr(3), sptr("Classic"), sptr("Jane@Example.com")
	b := entry("PD-1", "perDiem", "trv-1", "2026-10-15", "USD", 200)
	b.Country, b.Paid, b.TravelAgentID, b.TravelAgentName, b.TravellerEmail = sptr("India"), "paid", iptr(3), sptr("Classic"), sptr("jane@example.com")
	c := entry("CC-1", "card", "", "2026-10-16", "EUR", 100) // unlinked, EUR has no rate
	c.TravellerEmail = sptr("bob@example.com")
	d := entry("EC-1", "claim", "TRV-2", "2026-11-02", "USD", 50)
	d.Country, d.Paid = sptr("France"), "nonPaid"
	e := entry("CC-2", "card", "", "2026-10-20", "LKR", 15000)
	e.CCCurrency, e.CCAmount = "USD", 48.2
	svc, _ := costService([]engagemententity.DashboardCostEntry{a, b, c, d, e}, &fakeRates{rates: map[string]float64{"LKR": 300}})
	s := mustSummary(t, svc, finPerms(39, 41), cq)

	// 300 + 200 + 50 + 48.2 USD; the EUR card line has no rate.
	if s.Kpis.Total.Value != 598.2 || s.Kpis.Trips.Value != 2 || s.Kpis.Travellers.Value != 2 || s.Kpis.UnlinkedSpend.Value != 48.2 {
		t.Errorf("kpis = %+v", s.Kpis)
	}
	// Average per trip: lines with a job number (550) over 2 trips.
	if s.Kpis.AveragePerTrip.Value != 275 {
		t.Errorf("average per trip = %v", s.Kpis.AveragePerTrip.Value)
	}
	sum := 0.0
	for _, bk := range s.BySource {
		sum += bk.Value
	}
	if dashboardRound(sum) != s.Kpis.Total.Value || len(s.BySource) != 4 {
		t.Errorf("bySource %+v does not add up to the total", s.BySource)
	}
	if len(s.NotConverted) != 1 || s.NotConverted[0] != (domain.CostNotConverted{Source: "card", Currency: "EUR", Amount: 100, Lines: 1}) {
		t.Errorf("notConverted = %+v", s.NotConverted)
	}
	if s.Compare == nil || s.Compare.From != "2025-01-01" || s.Compare.To != "2025-11-05" || s.Currency != "USD" || s.RateRule != "monthEnd" {
		t.Errorf("compare/currency = %+v %s", s.Compare, s.Currency)
	}
	// Paid 500 / (500 + 50).
	if s.PaidSharePercent == nil || *s.PaidSharePercent != 90.9 {
		t.Errorf("paid share = %v", s.PaidSharePercent)
	}
	if len(s.ByPaid) != 3 || s.ByPaid[0].Key != "paid" || s.ByPaid[2].Key != "unknown" {
		t.Errorf("byPaid = %+v", s.ByPaid)
	}
	// Paid lines only; the per diem has no customer on record.
	if len(s.ByCustomer) != 2 || s.ByCustomer[0].Label != "Acme" || *s.ByCustomer[0].Trips != 1 || s.ByCustomer[1].Label != "Unknown" {
		t.Errorf("byCustomer = %+v", s.ByCustomer)
	}
	var agent *domain.CostBucket
	for i := range s.ByTravelAgent {
		if s.ByTravelAgent[i].Key == "3" {
			agent = &s.ByTravelAgent[i]
		}
	}
	if agent == nil || *agent.Trips != 1 || *agent.AveragePerTrip != 500 {
		t.Errorf("byTravelAgent = %+v", s.ByTravelAgent)
	}
	for _, ct := range s.ByCostType {
		if (ct.Key == "Card" || ct.Key == "Claim") == *ct.Categorised {
			t.Errorf("categorised wrong for %+v", ct)
		}
	}
	if len(s.ByMonth) != 11 || s.ByMonth[9].Month != "2026-10" || s.ByMonth[9].Values["Travel Invoices"] != 300 {
		t.Errorf("byMonth = %+v", s.ByMonth[9])
	}
	if len(s.Facets.Countries) != 3 || s.Facets.Countries[2] != "Unknown" || len(s.Facets.TravelAgents) != 1 {
		t.Errorf("facets = %+v", s.Facets)
	}
}

func TestTravelCosts_ThinRuleTopTenAndOther(t *testing.T) {
	var cur, prev []engagemententity.DashboardCostEntry
	for i := 0; i < 12; i++ {
		e := entry(fmt.Sprintf("INV-%d", i), "invoice", "", "2026-10-01", "USD", float64(100-i))
		e.Country = sptr("C" + strconv.Itoa(i))
		cur = append(cur, e)
	}
	// Last year only C0 (so C0 compares, the rest are thin) and C11 (which falls into Other).
	p0 := entry("INV-p0", "invoice", "", "2025-10-01", "USD", 50)
	p0.Country = sptr("C0")
	p11 := entry("INV-p11", "invoice", "", "2025-10-01", "USD", 10)
	p11.Country = sptr("C11")
	prev = append(prev, p0, p11)
	f := &fakeDashboardEntity{costFn: func(fl engagemententity.DashboardCostLinesFilter) *engagemententity.DashboardCostLines {
		if strings.HasPrefix(fl.From, "2025") {
			return &engagemententity.DashboardCostLines{Items: prev}
		}
		return &engagemententity.DashboardCostLines{Items: cur}
	}}
	svc := NewDashboardService(f, nil, nil, dashboardNow)
	s := mustSummary(t, svc, finPerms(39, 41), cq)
	if len(s.ByCountry) != 11 || s.ByCountry[10].Label != "Other" || s.ByCountry[10].Key != "" || s.ByCountry[10].Value != 89+90 {
		t.Fatalf("byCountry = %+v", s.ByCountry)
	}
	if s.ByCountry[10].Previous == nil || *s.ByCountry[10].Previous != 10 {
		t.Errorf("Other previous = %v", s.ByCountry[10].Previous)
	}
	c0 := s.ByCountry[0]
	if c0.Thin || c0.ChangePercent == nil || *c0.ChangePercent != 100 || *c0.Previous != 50 {
		t.Errorf("C0 = %+v, want +100%% on 50", c0)
	}
	if c1 := s.ByCountry[1]; !c1.Thin || c1.ChangePercent != nil {
		t.Errorf("C1 has no previous year: %+v", c1)
	}
	// More than five times last year is thin too.
	if p, c, thin := costChange(501, 100, true); !thin || c != nil || *p != 100 {
		t.Errorf("501 vs 100: thin %v change %v", thin, c)
	}
	if _, c, thin := costChange(500, 100, true); thin || *c != 400 {
		t.Errorf("500 vs 100: thin %v change %v", thin, c)
	}
	if p, c, thin := costChange(5, 0, false); p != nil || c != nil || thin {
		t.Error("no comparison must give nothing")
	}
	// compare=false makes one entity call and no previous figures.
	f.costCalls = 0
	svc = NewDashboardService(f, nil, nil, dashboardNow)
	s = mustSummary(t, svc, finPerms(39, 41), CostQuery{From: cq.From, To: cq.To})
	if f.costCalls != 1 || s.Compare != nil || s.Kpis.Total.Previous != nil {
		t.Errorf("compare=false: %d calls, %+v", f.costCalls, s.Compare)
	}
}

func TestShiftYear(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	for in, want := range map[string]string{"2028-02-29": "2027-02-28", "2026-11-24": "2025-11-24", "2028-03-01": "2027-03-01", "2025-02-28": "2024-02-28"} {
		if got := shiftYear(d(in)).Format("2006-01-02"); got != want {
			t.Errorf("shiftYear(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestTravelCosts_Validation(t *testing.T) {
	svc, _ := costService(nil, nil)
	bad := []CostQuery{
		{From: "2026-01-01"},
		{To: "2026-01-01"},
		{From: "bad", To: "2026-01-01"},
		{From: "2026-02-01", To: "2026-01-01"},
		{From: "2024-01-01", To: "2026-11-05"},
		{From: "2026-01-01", To: "2026-02-01", Filter: CostFilter{Sources: []string{"cash"}}},
		{From: "2026-01-01", To: "2026-02-01", Filter: CostFilter{Paid: []string{"maybe"}}},
		{From: "2026-01-01", To: "2026-02-01", Filter: CostFilter{Employees: make([]string, 51)}},
	}
	for i, q := range bad {
		if _, err := svc.TravelCosts(context.Background(), finPerms(39, 41), q); !dashboardIsValidation(err) {
			t.Errorf("case %d (%+v): err = %v", i, q, err)
		}
	}
	// No dates: this year to date. 731 days exactly is fine.
	s := mustSummary(t, svc, finPerms(39, 41), CostQuery{})
	if s.From != "2026-01-01" || s.To != "2026-11-05" {
		t.Errorf("default range = %s to %s", s.From, s.To)
	}
	mustSummary(t, svc, finPerms(39, 41), CostQuery{From: "2024-11-05", To: "2026-11-05"})
	if _, err := svc.CostTransactions(context.Background(), finPerms(39, 41), cq, CostTransactionsQuery{GroupBy: "colour"}); !dashboardIsValidation(err) {
		t.Errorf("groupBy: %v", err)
	}
	if _, err := svc.CostTransactions(context.Background(), finPerms(39, 41), cq, CostTransactionsQuery{Sort: "size"}); !dashboardIsValidation(err) {
		t.Errorf("sort: %v", err)
	}
}

func TestTravelCosts_Authorisation(t *testing.T) {
	lines := []engagemententity.DashboardCostEntry{
		entry("INV-1", "invoice", "T1", "2026-10-01", "USD", 10), entry("PD-1", "perDiem", "T1", "2026-10-01", "USD", 20),
		entry("CC-1", "card", "T2", "2026-10-01", "USD", 30), entry("EC-1", "claim", "T2", "2026-10-01", "USD", 40),
	}
	lines[2].TravellerEmail = sptr("cardholder@example.com")
	svc, _ := costService(lines, nil)
	ctx := context.Background()

	// A Travel Super Admin who is in no finance group gets all four sources, the employee filter and both CSVs.
	tsa := tsaPerms()
	s := mustSummary(t, svc, tsa, cq)
	if s.Kpis.Total.Value != 100 || len(s.BySource) != 4 {
		t.Errorf("tsa total = %v sources %d", s.Kpis.Total.Value, len(s.BySource))
	}
	byEmp := cq
	byEmp.Filter.Employees = []string{"cardholder@example.com"}
	if s := mustSummary(t, svc, tsa, byEmp); s.Kpis.Total.Value != 30 {
		t.Errorf("employee filter total = %v", s.Kpis.Total.Value)
	}
	if c, err := svc.TravelCostsCSV(ctx, tsa, cq, ""); err != nil || len(c.Rows) < 10 {
		t.Errorf("tsa summary csv: %v", err)
	}
	if c, err := svc.CostTransactionsCSV(ctx, tsa, cq); err != nil || len(c.Rows) < 2 {
		t.Errorf("tsa transactions csv: %v", err)
	}
	if page, err := svc.CostTransactions(ctx, tsa, cq, CostTransactionsQuery{}); err != nil || page.(domain.CostLinePage).Total != 4 {
		t.Errorf("tsa transactions: %+v %v", page, err)
	}

	// Neither finance nor Travel Super Admin: 403 on all routes (and the CSVs).
	for name, p := range map[string]domain.Perms{
		"employee":      dashboardPerms("e@wso2.com", dashboardEmployeeAlloc, dashboardEmployeeAlloc, dashboardEmployeeTravel, dashboardEmployeeTravel),
		"travel team":   dashboardPerms("t@wso2.com", nil, nil, []int{1, 39, 41}, []int{1, 39, 41}),
		"finance group": dashboardPerms("f@wso2.com", nil, nil, dashboardEmployeeTravel, dashboardEmployeeTravel, "app-travel-approver-finance"), // group without 39 or 41
		"account mgr":   dashboardPerms("a@wso2.com", []int{1, 2}, []int{1, 2}, nil, nil),
	} {
		if _, err := svc.TravelCosts(ctx, p, cq); !isForbidden(err) {
			t.Errorf("%s: summary err = %v", name, err)
		}
		if _, err := svc.CostTransactions(ctx, p, cq, CostTransactionsQuery{}); !isForbidden(err) {
			t.Errorf("%s: transactions err = %v", name, err)
		}
		if _, err := svc.TravelCostsCSV(ctx, p, cq, ""); !isForbidden(err) {
			t.Errorf("%s: csv err = %v", name, err)
		}
	}

	// A finance caller without 39/30 loses invoices, without 41/42 loses per diems; cards and claims stay.
	for _, c := range []struct {
		own  []int
		want float64
		n    int
	}{{[]int{39}, 80, 3}, {[]int{41}, 90, 3}, {[]int{39, 41}, 100, 4}, {[]int{39, 30}, 80, 3}, {[]int{41, 42}, 90, 3}} {
		p := finPerms(c.own...)
		s := mustSummary(t, svc, p, cq)
		if s.Kpis.Total.Value != c.want || len(s.BySource) != c.n {
			t.Errorf("own %v: total %v sources %d, want %v / %d", c.own, s.Kpis.Total.Value, len(s.BySource), c.want, c.n)
		}
		// Asking for a source they may not see is ignored, never an error.
		q := cq
		q.Filter.Sources = []string{"invoice", "perDiem", "card"}
		if s := mustSummary(t, svc, p, q); s.Kpis.Total.Value > c.want-40 {
			t.Errorf("own %v: source filter total %v includes claims or hidden sources", c.own, s.Kpis.Total.Value)
		}
	}
}

func TestTravelCosts_FailedSourcesAndTruncation(t *testing.T) {
	f := &fakeDashboardEntity{costResp: &engagemententity.DashboardCostLines{
		Items:       []engagemententity.DashboardCostEntry{entry("INV-1", "invoice", "T1", "2026-10-01", "USD", 10)},
		Unavailable: []string{"card", "claim"}, Truncated: []string{"invoice"},
	}}
	svc := NewDashboardService(f, nil, nil, dashboardNow)
	s := mustSummary(t, svc, finPerms(39, 41), cq)
	if len(s.FailedSources) != 2 || s.FailedSources[0] != "card" || !s.Truncated || s.Kpis.Total.Value != 10 {
		t.Errorf("failedSources %v truncated %v total %v", s.FailedSources, s.Truncated, s.Kpis.Total.Value)
	}
}

func TestCostApply_Filters(t *testing.T) {
	mk := func(id, src, job, country, customer, paid, ct string, agent *int, email string) costLine {
		e := entry(id, src, job, "2026-10-01", "USD", 10)
		e.Paid, e.CostType, e.TravelAgentID = paid, ct, agent
		if country != "" {
			e.Country = sptr(country)
		}
		if customer != "" {
			e.Customer = sptr(customer)
		}
		l := costLine{e: e, converted: true, usd: dashboardUSD{USD: 10}, job: strings.ToUpper(job), month: "2026-10"}
		if email != "" {
			l.traveller = &domain.Person{Email: email}
		}
		return l
	}
	lines := []costLine{
		mk("1", "invoice", "T1", "India", "Acme", "paid", "Hotel", iptr(3), "a@x.com"),
		mk("2", "card", "", "", "", "unknown", "Card", nil, "b@x.com"),
		mk("3", "claim", "t2", "France", "Acme", "nonPaid", "Claim", iptr(4), ""),
	}
	ids := func(f CostFilter) string {
		var out []string
		for _, l := range costApply(lines, f) {
			out = append(out, l.e.ID)
		}
		return strings.Join(out, ",")
	}
	for name, c := range map[string]struct {
		f    CostFilter
		want string
	}{
		"none":        {CostFilter{}, "1,2,3"},
		"source":      {CostFilter{Sources: []string{"card", "claim"}}, "2,3"},
		"employee":    {CostFilter{Employees: []string{"A@x.com"}}, "1"},
		"job":         {CostFilter{JobNumbers: []string{" t1", "T2"}}, "1,3"},
		"country":     {CostFilter{Countries: []string{"india"}}, "1"},
		"unknown":     {CostFilter{Countries: []string{"Unknown"}}, "2"},
		"agent":       {CostFilter{TravelAgents: []int{4}}, "3"},
		"no agent":    {CostFilter{TravelAgents: []int{0}}, "2"},
		"paid":        {CostFilter{Paid: []string{"paid", "unknown"}}, "1,2"},
		"customer":    {CostFilter{Customers: []string{"acme"}}, "1,3"},
		"cost type":   {CostFilter{CostTypes: []string{"claim"}}, "3"},
		"unlinked":    {CostFilter{Unlinked: true}, "2"},
		"two filters": {CostFilter{Customers: []string{"Acme"}, Paid: []string{"paid"}}, "1"},
	} {
		if got := ids(c.f); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestCostGroupsAndTransactions(t *testing.T) {
	a := entry("INV-1", "invoice", "TRV-1", "2026-10-14", "USD", 100)
	a.Country, a.TravellerEmail = sptr("India"), sptr("jane@x.com")
	b := entry("PD-1", "perDiem", "TRV-1", "2026-10-15", "USD", 50)
	b.Country, b.TravellerEmail = sptr("India"), sptr("jane@x.com")
	c := entry("CC-1", "card", "", "2026-09-01", "USD", 10)
	c.TravellerEmail = sptr("bob@x.com")
	svc, _ := costService([]engagemententity.DashboardCostEntry{a, b, c}, nil)
	ctx := context.Background()
	out, err := svc.CostTransactions(ctx, finPerms(39, 41), cq, CostTransactionsQuery{GroupBy: "country", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	gp := out.(domain.CostGroupPage)
	if gp.Total != 2 || gp.Groups[0].Key != "India" || gp.Groups[0].Total != 150 || gp.Groups[0].Lines != 2 || gp.Groups[0].Trips != 1 || gp.Groups[0].Travellers != 1 || gp.Groups[1].Key != "Unknown" {
		t.Errorf("groups = %+v", gp)
	}
	out, _ = svc.CostTransactions(ctx, finPerms(39, 41), cq, CostTransactionsQuery{GroupBy: "month", Limit: 1, Offset: 1, Sort: "date"})
	if gp := out.(domain.CostGroupPage); gp.Total != 2 || len(gp.Groups) != 1 || gp.Groups[0].Key != "2026-10" {
		t.Errorf("month groups page = %+v", gp)
	}
	// Lines: default newest first; paging; totals cover every filtered line.
	out, _ = svc.CostTransactions(ctx, finPerms(39, 41), cq, CostTransactionsQuery{Limit: 2})
	lp := out.(domain.CostLinePage)
	if lp.Total != 3 || lp.TotalUSD != 160 || len(lp.Items) != 2 || lp.Items[0].ID != "PD-1" {
		t.Errorf("lines = %+v", lp)
	}
	out, _ = svc.CostTransactions(ctx, finPerms(39, 41), cq, CostTransactionsQuery{Limit: 5, Sort: "-amount"})
	if lp := out.(domain.CostLinePage); lp.Items[0].ID != "INV-1" || *lp.Items[0].Rate != 1 || *lp.Items[0].RateSource != "recorded" {
		t.Errorf("by amount = %+v", lp.Items[0])
	}
}

// The summary, the transactions and both CSVs read one cached set, so their
// totals agree, and trimming for a caller never changes the cached lines.
func TestCostTotalsAgreeAndCacheIsNotMutated(t *testing.T) {
	lines := []engagemententity.DashboardCostEntry{
		entry("INV-1", "invoice", "T1", "2026-10-01", "LKR", 90000), entry("PD-1", "perDiem", "T1", "2026-10-02", "USD", 20),
		entry("CC-1", "card", "T2", "2026-10-03", "USD", 30), entry("EC-1", "claim", "", "2026-10-04", "USD", 40),
	}
	svc, f := costService(lines, &fakeRates{rates: map[string]float64{"LKR": 300}})
	ctx := context.Background()
	q := cq
	q.Compare = false
	// A finance caller who only sees cards and claims first, then a Travel Super Admin: the cached set is the same.
	fin := finPerms(41)
	s1 := mustSummary(t, svc, fin, q)
	if s1.Kpis.Total.Value != 20+30+40 { // per diem, card, claim
		t.Errorf("finance total = %v", s1.Kpis.Total.Value)
	}
	s2 := mustSummary(t, svc, tsaPerms(), q)
	if s2.Kpis.Total.Value != 300+20+30+40 {
		t.Errorf("tsa total = %v (the cached set was trimmed for the first caller?)", s2.Kpis.Total.Value)
	}
	page, _ := svc.CostTransactions(ctx, tsaPerms(), q, CostTransactionsQuery{})
	if page.(domain.CostLinePage).TotalUSD != s2.Kpis.Total.Value {
		t.Error("transactions total differs from the summary")
	}
	srcSum := 0.0
	for _, b := range s2.BySource {
		srcSum += b.Value
	}
	if dashboardRound(srcSum) != s2.Kpis.Total.Value {
		t.Error("bySource does not add up")
	}
	csvT, _ := svc.CostTransactionsCSV(ctx, tsaPerms(), q)
	// The file is a plain table: the header first, then one row per line, no total row.
	if csvT.Rows[0][0] != "Date" || len(csvT.Rows) != 5 {
		t.Errorf("transactions csv = %v", csvT.Rows)
	}
	var cents int64
	for _, r := range csvT.Rows[1:] {
		cents += csvCents(t, r[12])
	}
	if cents != 39000 {
		t.Errorf("transactions rows sum %d cents, want 39000", cents)
	}
	if f.costCalls != 1 {
		t.Errorf("entity calls = %d, want one for the whole sequence (60 s cached set)", f.costCalls)
	}
	// The cached lines still hold all four sources.
	set, _ := svc.(*dashboardService).costSetFor(ctx, q.From, q.To)
	if len(set.lines) != 4 {
		t.Errorf("cached set has %d lines", len(set.lines))
	}
}

func csvText(c *CostCSV) string {
	text := ""
	for _, r := range c.Rows {
		text += strings.Join(r, ",") + "\n"
	}
	return text
}

func TestCostCSV_HeaderAndSections(t *testing.T) {
	a := entry("INV-1", "invoice", "T1", "2026-10-01", "USD", 10)
	a.Customer, a.Paid, a.Country = sptr("=cmd|' /C calc'!A0"), "paid", sptr("India")
	svc, _ := costService([]engagemententity.DashboardCostEntry{a}, nil)
	ctx := context.Background()
	q := cq
	q.Filter.Sources = []string{"invoice"}
	c, err := svc.TravelCostsCSV(ctx, finPerms(39, 41), q, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Filename != "Travel_Costs_Summary_2026-01-01_2026-11-05.csv" || c.Rows[0][0] != "Report" || c.Rows[0][1] != "Travel Costs Summary" {
		t.Errorf("file %s first row %v", c.Filename, c.Rows[0])
	}
	text := csvText(c)
	for _, want := range []string{"Report,Travel Costs Summary", "Date range,2026-01-01 to 2026-11-05", "Compared with,2025-01-01 to 2025-11-05",
		"Filters,Source=Travel Invoices", "Amounts in,USD", "How USD is calculated,USD amounts as recorded. Others at NetSuite month-end rate (today's rate for this month)",
		"Data as of,2026-11-05 09:00 UTC",
		"Key Figures\nFigure,This Period,Same Period Last Year,Change vs Last Year (%)", "Spend by Source\nSource,This Period (USD),Same Period Last Year (USD),Change vs Last Year (%)",
		"Spend by Destination Country\nCountry,This Period (USD),Same Period Last Year (USD),Change vs Last Year (%)",
		"Spend by Paid Customer\nCustomer,This Period (USD),Same Period Last Year (USD),Change vs Last Year (%),Trips",
		"Travel Agent Comparison\nTravel Agent,This Period (USD),Same Period Last Year (USD),Change vs Last Year (%),Trips,Average per Trip (USD)",
		"Spend by Month\nMonth,Travel Invoices (USD),Per Diems (USD),Credit Card Expenses (USD),Expense Claims (USD),Total (USD)"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary csv lacks %q:\n%s", want, text)
		}
	}
	// A blank row separates the header block from the sections; no "Item" header, no rate rule row, no hint row.
	if c.Rows[6][0] != "Data as of" || len(c.Rows[7]) != 0 || strings.Contains(text, "Item,") || strings.Contains(text, "Rate rule") || strings.Contains(text, "Currency,") {
		t.Errorf("header block shape:\n%s", text)
	}
	// Column A and every cell are short and there is no semicolon anywhere.
	for _, r := range c.Rows {
		for _, cell := range r {
			if len(cell) > 100 || strings.Contains(cell, ";") {
				t.Errorf("long or semicolon cell %q", cell)
			}
		}
	}
	one, err := svc.TravelCostsCSV(ctx, finPerms(39, 41), cq, "byCountry")
	if err != nil || one.Filename != "Travel_Costs_By_Country_2026-01-01_2026-11-05.csv" || strings.Contains(fmt.Sprint(one.Rows), "Spend by Source") || one.Rows[0][1] != "Travel Costs: Spend by Destination Country" {
		t.Errorf("section csv: %v %v", one, err)
	}
	if _, err := svc.TravelCostsCSV(ctx, finPerms(39, 41), cq, "nope"); !dashboardIsValidation(err) {
		t.Errorf("bad section: %v", err)
	}
}

// With the comparison off the Last Year and Change columns are not printed at all.
func TestCostCSV_NoComparisonLeavesOutTheColumns(t *testing.T) {
	a := entry("INV-1", "invoice", "T1", "2026-10-01", "USD", 10)
	svc, _ := costService([]engagemententity.DashboardCostEntry{a}, nil)
	q := cq
	q.Compare = false
	c, err := svc.TravelCostsCSV(context.Background(), finPerms(39, 41), q, "")
	if err != nil {
		t.Fatal(err)
	}
	text := csvText(c)
	if strings.Contains(text, "Last Year") || strings.Contains(text, "Change vs") || !strings.Contains(text, "Compared with,Not compared") ||
		!strings.Contains(text, "Figure,This Period\n") || !strings.Contains(text, "Source,This Period (USD)\n") ||
		!strings.Contains(text, "Customer,This Period (USD),Trips\n") {
		t.Errorf("compare off:\n%s", text)
	}
	for _, r := range c.Rows {
		if len(r) > 1 && r[len(r)-1] == "" && r[0] != "Filters" {
			t.Errorf("empty trailing cell in %v", r)
		}
	}
}

// Every total in the summary file equals the sum of its rows to the cent.
func TestCostCSV_SummaryTotalsEqualTheRows(t *testing.T) {
	var lines []engagemententity.DashboardCostEntry
	for i := 0; i < 120; i++ {
		src := []string{"invoice", "perDiem", "card", "claim"}[i%4]
		lines = append(lines, entry(fmt.Sprintf("L-%d", i), src, "", fmt.Sprintf("2026-0%d-%02d", 1+i%9, 1+i%28), "LKR", float64(173+i*37)+0.19))
	}
	svc, _ := costService(lines, &fakeRates{rates: map[string]float64{"LKR": 311.377}})
	q := cq
	q.Compare = false
	c, err := svc.TravelCostsCSV(context.Background(), tsaPerms(), q, "byMonth")
	if err != nil {
		t.Fatal(err)
	}
	var head []string
	var grand, rowTotals int64
	for _, r := range c.Rows {
		if len(r) == 6 && r[0] == "Month" {
			head = r
			continue
		}
		if head == nil || len(r) != 6 {
			continue
		}
		var sum int64
		for _, v := range r[1:5] {
			sum += csvCents(t, v)
		}
		if sum != csvCents(t, r[5]) {
			t.Errorf("%s: sources sum %d, total %s", r[0], sum, r[5])
		}
		rowTotals += csvCents(t, r[5])
	}
	k, _ := svc.TravelCostsCSV(context.Background(), tsaPerms(), q, "kpis")
	for _, r := range k.Rows {
		if len(r) > 1 && r[0] == "Total Spend (USD)" {
			grand = csvCents(t, r[1])
		}
	}
	if grand == 0 || grand != rowTotals {
		t.Errorf("key figure total %d cents, month rows %d", grand, rowTotals)
	}
}

// The transactions file is a pure table with the agreed columns, in order.
func TestCostTransactionsCSV_IsAPureTable(t *testing.T) {
	a := entry("INV-1", "invoice", "TRV-1", "2026-10-01", "LKR", 90000)
	a.Country, a.Customer, a.Paid, a.TravellerEmail = sptr(" India"), sptr("Acme, Inc"), "paid", sptr("jane@x.com")
	b := entry("CC-1", "card", "", "2026-10-02", "LKR", 15000)
	b.CCCurrency, b.CCAmount = "USD", 48.2
	c := entry("PD-1", "perDiem", "TRV-1", "2026-10-03", "USD", 20)
	d := entry("EC-1", "claim", "", "2026-10-04", "EUR", 5) // no rate
	svc, _ := costService([]engagemententity.DashboardCostEntry{a, b, c, d}, &fakeRates{rates: map[string]float64{"LKR": 300}})
	tx, err := svc.CostTransactionsCSV(context.Background(), finPerms(39, 41), cq)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Date", "Source", "Reference", "Travel Job Number", "Employee", "Email", "Destination Country", "Customer", "Paid / Non-Paid",
		"Cost Type", "Amount", "Currency", "Amount (USD)", "Rate Used", "Rate Date", "How Converted"}
	if fmt.Sprint(tx.Rows[0]) != fmt.Sprint(want) || len(tx.Rows) != 5 {
		t.Fatalf("rows = %v", tx.Rows)
	}
	for _, r := range tx.Rows {
		if len(r) != len(want) {
			t.Errorf("ragged row %v", r)
		}
	}
	by := map[string][]string{}
	for _, r := range tx.Rows[1:] {
		by[r[2]] = r
	}
	if r := by["INV-1"]; r[0] != "2026-10-01" || r[6] != "India" || r[10] != "90000.00" || r[11] != "LKR" || r[12] != "300.00" || r[13] != "300" || r[14] == "" || r[15] != "NetSuite month-end rate" {
		t.Errorf("converted row %v", r)
	}
	if r := by["CC-1"]; r[12] != "48.20" || r[15] != "Billed in USD" {
		t.Errorf("card row %v", r)
	}
	if r := by["PD-1"]; r[12] != "20.00" || r[15] != "Recorded in USD" || r[13] != "1" || r[14] != "" {
		t.Errorf("usd row %v", r)
	}
	if r := by["EC-1"]; r[12] != "" || r[15] != "Not converted (no rate)" {
		t.Errorf("unconverted row %v", r)
	}
}

func TestTravelCosts_FixedBarsKeepLastYearsSpend(t *testing.T) {
	prevCard := entry("CC-p", "card", "T1", "2025-10-01", "USD", 100)
	f := &fakeDashboardEntity{costFn: func(fl engagemententity.DashboardCostLinesFilter) *engagemententity.DashboardCostLines {
		if strings.HasPrefix(fl.From, "2025") {
			return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{prevCard}}
		}
		return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{entry("INV-1", "invoice", "T1", "2026-10-01", "USD", 10)}}
	}}
	s := mustSummary(t, NewDashboardService(f, nil, nil, dashboardNow), finPerms(39, 41), cq)
	for _, b := range s.BySource {
		if b.Key != "card" {
			continue
		}
		if b.Value != 0 || b.Previous == nil || *b.Previous != 100 || b.ChangePercent == nil || *b.ChangePercent != -100 {
			t.Errorf("card bar = %+v, want 0 against 100 last year", b)
		}
	}
	for _, b := range s.ByPaid {
		if b.Key == "unknown" && (b.Previous == nil || *b.Previous != 100) {
			t.Errorf("unknown paid bar previous = %v, want 100", b.Previous)
		}
	}
}

func TestCostGroups_MonthIsComparedWithTheSameMonthLastYear(t *testing.T) {
	f := &fakeDashboardEntity{costFn: func(fl engagemententity.DashboardCostLinesFilter) *engagemententity.DashboardCostLines {
		if strings.HasPrefix(fl.From, "2025") {
			return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{entry("INV-p", "invoice", "T1", "2025-10-14", "USD", 100)}}
		}
		return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{entry("INV-1", "invoice", "T1", "2026-10-14", "USD", 200)}}
	}}
	out, err := NewDashboardService(f, nil, nil, dashboardNow).CostTransactions(context.Background(), finPerms(39, 41), cq, CostTransactionsQuery{GroupBy: "month", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	g := out.(domain.CostGroupPage).Groups[0]
	if g.Key != "2026-10" || g.Previous == nil || *g.Previous != 100 || g.ChangePercent == nil || *g.ChangePercent != 100 {
		t.Errorf("month group = %+v, want +100%% on 100 from 2025-10", g)
	}
}

func TestTravelCosts_IncompleteLastYearGivesNoComparisonAndTheCsvSaysSo(t *testing.T) {
	f := &fakeDashboardEntity{costFn: func(fl engagemententity.DashboardCostLinesFilter) *engagemententity.DashboardCostLines {
		if strings.HasPrefix(fl.From, "2025") {
			return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{entry("INV-p", "invoice", "T1", "2025-10-14", "USD", 100)}, Unavailable: []string{"card"}}
		}
		return &engagemententity.DashboardCostLines{Items: []engagemententity.DashboardCostEntry{entry("INV-1", "invoice", "T1", "2026-10-14", "USD", 200)}, Unavailable: []string{"claim"}}
	}}
	svc := NewDashboardService(f, nil, nil, dashboardNow)
	s := mustSummary(t, svc, finPerms(39, 41), cq)
	if len(s.ComparisonFailedSources) != 1 || s.ComparisonFailedSources[0] != "card" || s.Compare == nil {
		t.Errorf("comparison failed sources %v compare %v", s.ComparisonFailedSources, s.Compare)
	}
	if s.Kpis.Total.Value != 200 || s.Kpis.Total.Previous != nil || s.Kpis.Total.ChangePercent != nil {
		t.Errorf("total = %+v, want 200 with no comparison", s.Kpis.Total)
	}
	c, err := svc.TravelCostsCSV(context.Background(), finPerms(39, 41), cq, "")
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, r := range c.Rows {
		text += strings.Join(r, ",") + "\n"
	}
	if !strings.Contains(text, "Incomplete data,Expense Claims missing, Last year incomplete") && !strings.Contains(text, "Incomplete data,\"Expense Claims missing, Last year incomplete\"") {
		t.Errorf("csv header lacks the incomplete data row:\n%s", text)
	}
	// The transactions file has no preamble to carry a note: it is only the table.
	tx, err := svc.CostTransactionsCSV(context.Background(), finPerms(39, 41), cq)
	if err != nil || tx.Rows[0][0] != "Date" {
		t.Errorf("transactions csv: %v %v", tx.Rows, err)
	}
}

// Every line is rounded to cents once, when converted, so each total is the
// exact sum of the cent amounts shown on the lines and in the CSV (no 1.36
// gap on a few hundred small LKR claims).
func TestCostTotalsEqualTheSumOfTheRoundedLines(t *testing.T) {
	var lines []engagemententity.DashboardCostEntry
	srcs := []string{"claim", "claim", "claim", "invoice", "card"}
	for i := 0; i < 400; i++ {
		src := srcs[i%len(srcs)]
		day := 1 + i%28
		// Odd LKR amounts at a rate that never divides evenly.
		lines = append(lines, entry(fmt.Sprintf("L-%d", i), src, "", fmt.Sprintf("2026-10-%02d", day), "LKR", float64(137+i*41)+0.37))
	}
	svc, _ := costService(lines, &fakeRates{rates: map[string]float64{"LKR": 317.123}})
	ctx := context.Background()
	q := cq
	q.Compare = false

	toCents := func(s string) int64 {
		t.Helper()
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return int64(math.Round(f * 100))
	}

	page, err := svc.CostTransactions(ctx, tsaPerms(), q, CostTransactionsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// Whole list through the CSV (not paged).
	csvT, err := svc.CostTransactionsCSV(ctx, tsaPerms(), q)
	if err != nil {
		t.Fatal(err)
	}
	var csvCents int64
	var rowsSeen int
	if csvT.Rows[0][12] != "Amount (USD)" {
		t.Fatalf("header = %v", csvT.Rows[0])
	}
	for _, r := range csvT.Rows[1:] {
		csvCents += toCents(r[12])
		rowsSeen++
	}
	if rowsSeen != len(lines) {
		t.Fatalf("csv rows = %d, want %d", rowsSeen, len(lines))
	}
	if got := toCents(strconv.FormatFloat(page.(domain.CostLinePage).TotalUSD, 'f', 2, 64)); got != csvCents {
		t.Errorf("transactions header %d cents, rows %d", got, csvCents)
	}

	s := mustSummary(t, svc, tsaPerms(), q)
	if got := int64(math.Round(s.Kpis.Total.Value * 100)); got != csvCents {
		t.Errorf("kpi total %d cents, rows %d", got, csvCents)
	}
	var bySrc, byMonth, byType int64
	for _, b := range s.BySource {
		bySrc += int64(math.Round(b.Value * 100))
	}
	for _, m := range s.ByMonth {
		for _, v := range m.Values {
			byMonth += int64(math.Round(v * 100))
		}
	}
	for _, b := range s.ByCostType {
		byType += int64(math.Round(b.Value * 100))
	}
	for name, got := range map[string]int64{"bySource": bySrc, "byMonth": byMonth, "byCostType": byType} {
		if got != csvCents {
			t.Errorf("%s sums to %d cents, rows %d", name, got, csvCents)
		}
	}
	for _, g := range []string{"country", "month", "source"} {
		out, err := svc.CostTransactions(ctx, tsaPerms(), q, CostTransactionsQuery{GroupBy: g, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, gr := range out.(domain.CostGroupPage).Groups {
			sum += int64(math.Round(gr.Total * 100))
		}
		if sum != csvCents {
			t.Errorf("groupBy %s sums to %d cents, rows %d", g, sum, csvCents)
		}
	}
}

func csvCents(t *testing.T, v string) int64 {
	t.Helper()
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("not a number: %q", v)
	}
	return int64(math.Round(f * 100))
}

// TestCostCSV_WriteSamples writes one sample of every cost CSV when
// CSV_SAMPLE_DIR is set (for review in Numbers or Excel); otherwise it skips.
func TestCostCSV_WriteSamples(t *testing.T) {
	dir := os.Getenv("CSV_SAMPLE_DIR")
	if dir == "" {
		t.Skip("CSV_SAMPLE_DIR not set")
	}
	var lines []engagemententity.DashboardCostEntry
	countries := []string{"India", "Japan", "United Kingdom", "Sri Lanka"}
	customers := []string{"Acme, Inc", "Zürich Bank", "Globex"}
	for i := 0; i < 60; i++ {
		src := []string{"invoice", "perDiem", "card", "claim"}[i%4]
		year := 2026
		if i%3 == 0 {
			year = 2025
		}
		cur := []string{"LKR", "USD", "LKR", "USD"}[i%4]
		e := entry(fmt.Sprintf("%s-%d", map[string]string{"invoice": "INV", "perDiem": "PD", "card": "CC", "claim": "EC"}[src], i), src, fmt.Sprintf("TRV-%d", i%7), fmt.Sprintf("%d-%02d-%02d", year, 1+i%10, 1+i%27), cur, float64(1500+i*173)+0.37)
		e.Country, e.Customer, e.Paid = sptr(countries[i%4]), sptr(customers[i%3]), []string{"paid", "nonPaid", "unknown"}[i%3]
		e.TravelAgentID, e.TravelAgentName, e.TravellerEmail = iptr(1+i%2), sptr([]string{"Classic Travels", "Skyline"}[i%2]), sptr(fmt.Sprintf("user%d@example.com", i%5))
		if src == "card" && i%8 == 2 {
			e.CCCurrency, e.CCAmount = "USD", 48.2
		}
		lines = append(lines, e)
	}
	// Each period gets only its own year's lines, so the comparison columns differ.
	f := &fakeDashboardEntity{costFn: func(fl engagemententity.DashboardCostLinesFilter) *engagemententity.DashboardCostLines {
		var in []engagemententity.DashboardCostEntry
		for _, e := range lines {
			if e.Date >= fl.From && e.Date <= fl.To {
				in = append(in, e)
			}
		}
		return &engagemententity.DashboardCostLines{Items: in}
	}}
	svc := NewDashboardService(f, nil, &fakeRates{rates: map[string]float64{"LKR": 311.377}}, dashboardNow)
	ctx := context.Background()
	write := func(c *CostCSV, err error, name string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString("\xEF\xBB\xBF")
		w := csv.NewWriter(&b)
		for _, r := range c.Rows {
			_ = w.Write(r)
		}
		w.Flush()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := tsaPerms()
	c, err := svc.TravelCostsCSV(ctx, p, cq, "")
	write(c, err, c.Filename)
	for _, sec := range costSectionOrder {
		c, err := svc.TravelCostsCSV(ctx, p, cq, sec)
		write(c, err, c.Filename)
	}
	nq := cq
	nq.Compare = false
	c, err = svc.TravelCostsCSV(ctx, p, nq, "")
	write(c, err, "NoComparison_"+c.Filename)
	c, err = svc.CostTransactionsCSV(ctx, p, cq)
	write(c, err, c.Filename)
}

func TestCostPrepared_ComparisonIgnoresSourcesOutsideTheFilter(t *testing.T) {
	allowed := []string{"invoice", "card"}
	prev := &costSet{failed: []string{"card"}}
	cards := &costPrepared{allowed: allowed, selected: costSelected(allowed, []string{"invoice"}), set: &costSet{}, prevSet: prev}
	if !cards.comparable() {
		t.Fatal("a card failure must not block an invoice-only comparison")
	}
	inv := &costPrepared{allowed: allowed, selected: costSelected(allowed, []string{"Invoice"}), set: &costSet{}, prevSet: &costSet{failed: []string{"invoice"}}}
	if inv.comparable() {
		t.Fatal("an invoice failure must block an invoice comparison")
	}
	all := &costPrepared{allowed: allowed, selected: costSelected(allowed, nil), set: &costSet{}, prevSet: prev}
	if all.comparable() {
		t.Fatal("with no source filter a card failure must block the comparison")
	}
}

func TestCostPrepared_CurrentPeriodFailureBlocksTheComparison(t *testing.T) {
	allowed := []string{"invoice", "card"}
	prev := &costSet{}
	cur := &costSet{failed: []string{"card"}}
	if (&costPrepared{allowed: allowed, selected: allowed, set: cur, prevSet: prev}).comparable() {
		t.Fatal("a card set missing now would show last year's cards as a drop")
	}
	if (&costPrepared{allowed: allowed, selected: allowed, set: &costSet{}, prevSet: &costSet{ratesPending: true}}).comparable() {
		t.Error("a previous period with rates pending is not comparable")
	}
	if !(&costPrepared{allowed: allowed, selected: []string{"invoice"}, set: cur, prevSet: prev}).comparable() {
		t.Fatal("a card failure must not block an invoice-only comparison")
	}
	if !(&costPrepared{allowed: allowed, selected: allowed, set: cur}).comparable() {
		t.Fatal("with no comparison period there is nothing to block")
	}
}
