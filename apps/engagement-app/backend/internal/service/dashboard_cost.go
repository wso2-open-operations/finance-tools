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
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/apierror"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/clients/engagemententity"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/domain"
)

// Limits of the Travel Costs endpoints.
const (
	costMaxRangeDays = 731
	costMaxValues    = 50
	costEntityBudget = 15 * time.Second
	costTopN         = 10
)

// Labels of the four sources, in screen order (the series keys of byMonth).
var costSourceOrder = []string{domain.CostSourceInvoice, domain.CostSourcePerDiem, domain.CostSourceCard, domain.CostSourceClaim}

var costSourceLabel = map[string]string{
	domain.CostSourceInvoice: dashboardSeriesInvoices,
	domain.CostSourcePerDiem: dashboardSeriesPerDiems,
	domain.CostSourceCard:    dashboardSeriesCards,
	domain.CostSourceClaim:   dashboardSeriesClaims,
}

// CostFilter narrows the lines of a Travel Costs query. Empty fields do not
// filter.
type CostFilter struct {
	Sources      []string
	Employees    []string
	JobNumbers   []string
	Countries    []string
	TravelAgents []int
	Paid         []string
	Customers    []string
	CostTypes    []string
	Unlinked     bool
}

// CostQuery is the range, the comparison and the filter of a Travel Costs
// request. From and To are inclusive; both empty means this UTC year to date.
type CostQuery struct {
	From, To string
	Compare  bool
	Filter   CostFilter
}

// CostTransactionsQuery is the grouping, order and page of the transactions.
type CostTransactionsQuery struct {
	GroupBy string
	Sort    string
	Limit   int
	Offset  int
}

// CostCSV is a CSV answer: the file name and every row, header block first.
type CostCSV struct {
	Filename string
	Rows     [][]string
}

// ── The line set ────────────────────────────────────────────────────────────

// costLine is a cost line with its USD amount. It is immutable once cached.
type costLine struct {
	e         engagemententity.DashboardCostEntry
	traveller *domain.Person
	usd       dashboardUSD
	converted bool
	pending   bool   // unconverted and its rate is still being looked up
	job       string // upper-cased job number, blank when none
	month     string // YYYY-MM
}

// costSet is every line of a date range, all four sources, unfiltered and
// untrimmed. It is shared between callers, so it is never modified.
type costSet struct {
	lines     []costLine
	failed    []string
	truncated bool
	// ratesPending: some exchange rates were still loading, so lines that need
	// them are unconverted for now. Cached only briefly.
	ratesPending bool
	asOf         time.Time
}

func (s *dashboardService) costSetFor(ctx context.Context, from, to string) (*costSet, error) {
	return s.costCache.get(ctx, from+"|"+to, func() (*costSet, error) {
		// The load is shared with every caller that joined this key, so one caller leaving must not cancel it.
		lctx := context.WithoutCancel(ctx)
		ectx, cancel := context.WithTimeout(lctx, costEntityBudget)
		defer cancel()
		res, err := s.entity.DashboardCostLines(ectx, engagemententity.DashboardCostLinesFilter{From: from, To: to})
		if err != nil {
			return nil, err
		}
		return s.buildCostSet(lctx, res), nil
	})
}

// buildCostSet turns the entity's cost lines into costLines: traveller names
// filled, each line converted to USD and rounded to cents once, sorted by date.
// Travel Costs and the per-trip cost summary both use it, so they never differ.
func (s *dashboardService) buildCostSet(ctx context.Context, res *engagemententity.DashboardCostLines) *costSet {
	set := &costSet{failed: append([]string{}, res.Unavailable...), truncated: len(res.Truncated) > 0, asOf: s.now().UTC()}

	persons := map[string]*domain.Person{}
	var toFill []*domain.Person
	for _, e := range res.Items {
		if e.TravellerEmail == nil || strings.TrimSpace(*e.TravellerEmail) == "" {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(*e.TravellerEmail))
		if persons[k] == nil {
			persons[k] = &domain.Person{Email: k}
			toFill = append(toFill, persons[k])
		}
	}
	s.people.fillNames(ctx, toFill...)

	today := s.today()
	var pairs []ratePair
	for _, e := range res.Items {
		if usdNeedsRate(e.OriginalCurrency, e.CCCurrency) {
			pairs = append(pairs, ratePair{strings.ToUpper(strings.TrimSpace(e.OriginalCurrency)), dashboardRateDate(monthOf(e.Date), today)})
		}
	}
	rs, pending := s.rates.resolve(ctx, pairs)
	set.ratesPending = pending

	set.lines = make([]costLine, 0, len(res.Items))
	for _, e := range res.Items {
		l := costLine{e: e, month: monthOf(e.Date)}
		if e.JobNumber != nil {
			l.job = strings.ToUpper(strings.TrimSpace(*e.JobNumber))
		}
		if e.TravellerEmail != nil {
			l.traveller = persons[strings.ToLower(strings.TrimSpace(*e.TravellerEmail))]
		}
		l.usd, l.converted = rs.usd(e.OriginalCurrency, e.OriginalAmount, e.CCCurrency, e.CCAmount, l.month, today)
		if !l.converted && pending && usdNeedsRate(e.OriginalCurrency, e.CCCurrency) {
			l.pending = s.rates.isPending(ratePair{strings.ToUpper(strings.TrimSpace(e.OriginalCurrency)), dashboardRateDate(l.month, today)})
		}
		set.lines = append(set.lines, l)
	}
	sort.SliceStable(set.lines, func(i, j int) bool {
		a, b := set.lines[i].e, set.lines[j].e
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		return a.ID < b.ID
	})
	return set
}

func monthOf(date string) string {
	if len(date) >= 7 {
		return date[:7]
	}
	return date
}

// ── Who may see what ────────────────────────────────────────────────────────

// costSources returns the sources the caller may see and false when they may
// not see Travel Costs at all (403). A Travel Super Admin sees all four; a
// finance caller sees invoices with own Travel 39 or 30, per diems with 41 or
// 42, and always cards and claims.
func costSources(p domain.Perms) ([]string, bool) {
	if domain.HasRole(p.TravelRoles, domain.TravelSuperAdminGroup) {
		return append([]string(nil), costSourceOrder...), true
	}
	if !(dashboardHasCardRole(p) && p.TravelOwn.Any(39, 41)) {
		return nil, false
	}
	var out []string
	if p.TravelOwn.Any(39, 30) {
		out = append(out, domain.CostSourceInvoice)
	}
	if p.TravelOwn.Any(41, 42) {
		out = append(out, domain.CostSourcePerDiem)
	}
	return append(out, domain.CostSourceCard, domain.CostSourceClaim), true
}

var errCostForbidden = &apierror.ForbiddenError{Msg: "You do not have access to travel costs."}

// ── Resolving a query ───────────────────────────────────────────────────────

type costPeriod struct{ from, to time.Time }

func (c costPeriod) strings() (string, string) {
	return c.from.Format(dashboardDateLayout), c.to.Format(dashboardDateLayout)
}

// shiftYear is the same day a year earlier; 29 February becomes 28 February.
func shiftYear(t time.Time) time.Time {
	if t.Month() == time.February && t.Day() == 29 {
		return time.Date(t.Year()-1, time.February, 28, 0, 0, 0, 0, time.UTC)
	}
	return t.AddDate(-1, 0, 0)
}

// resolve validates q and returns the period and, when q.Compare is set,
// the same period one year earlier.
func (s *dashboardService) resolve(q CostQuery) (cur costPeriod, prev *costPeriod, err error) {
	today := s.today()
	switch {
	case q.From == "" && q.To == "":
		cur = costPeriod{time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, time.UTC), today}
	case q.From == "" || q.To == "":
		return cur, nil, &apierror.ValidationError{Msg: "from and to go together."}
	default:
		f, e1 := time.Parse(dashboardDateLayout, q.From)
		t, e2 := time.Parse(dashboardDateLayout, q.To)
		if e1 != nil {
			return cur, nil, &apierror.ValidationError{Msg: "from must be a date (YYYY-MM-DD)."}
		}
		if e2 != nil {
			return cur, nil, &apierror.ValidationError{Msg: "to must be a date (YYYY-MM-DD)."}
		}
		cur = costPeriod{f, t}
	}
	if cur.to.Before(cur.from) {
		return cur, nil, &apierror.ValidationError{Msg: "from must not be after to."}
	}
	if cur.to.Sub(cur.from) > costMaxRangeDays*24*time.Hour {
		return cur, nil, &apierror.ValidationError{Msg: "The range is at most 731 days."}
	}
	if q.Compare {
		pp := costPeriod{shiftYear(cur.from), shiftYear(cur.to)}
		prev = &pp
	}
	if err := q.Filter.validate(); err != nil {
		return cur, nil, err
	}
	return cur, prev, nil
}

func (f CostFilter) validate() error {
	counts := map[string]int{
		"source": len(f.Sources), "employee": len(f.Employees), "jobNumber": len(f.JobNumbers), "country": len(f.Countries),
		"travelAgent": len(f.TravelAgents), "paid": len(f.Paid), "customer": len(f.Customers), "costType": len(f.CostTypes),
	}
	for name, n := range counts {
		if n > costMaxValues {
			return &apierror.ValidationError{Msg: name + ": at most 50 values."}
		}
	}
	for _, v := range f.Sources {
		if costSourceLabel[v] == "" {
			return &apierror.ValidationError{Msg: "source must be invoice, perDiem, card or claim."}
		}
	}
	for _, v := range f.Paid {
		if v != domain.CostPaid && v != domain.CostNonPaid && v != domain.CostUnknown {
			return &apierror.ValidationError{Msg: "paid must be paid, nonPaid or unknown."}
		}
	}
	return nil
}

// loadSets reads the current and, when asked, the previous period in parallel.
func (s *dashboardService) loadSets(ctx context.Context, cur costPeriod, prev *costPeriod) (*costSet, *costSet, error) {
	type res struct {
		set *costSet
		err error
	}
	ch := make(chan res, 1)
	if prev != nil {
		go func() {
			f, t := prev.strings()
			set, err := s.costSetFor(ctx, f, t)
			ch <- res{set, err}
		}()
	}
	f, t := cur.strings()
	curSet, err := s.costSetFor(ctx, f, t)
	var prevSet *costSet
	if prev != nil {
		r := <-ch
		if err == nil {
			err = r.err
		}
		prevSet = r.set
	}
	if err != nil {
		return nil, nil, err
	}
	return curSet, prevSet, nil
}

// ── Filtering ───────────────────────────────────────────────────────────────

func lowerSet(vals []string) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return out
}

// costTrim returns the lines of the sources the caller may see, as a new slice.
func costTrim(lines []costLine, allowed []string) []costLine {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	out := make([]costLine, 0, len(lines))
	for _, l := range lines {
		if ok[l.e.Source] {
			out = append(out, l)
		}
	}
	return out
}

func costCountry(l costLine) string {
	if l.e.Country == nil || strings.TrimSpace(*l.e.Country) == "" {
		return "Unknown"
	}
	return strings.TrimSpace(*l.e.Country)
}

func costCustomer(l costLine) string {
	if l.e.Customer == nil || strings.TrimSpace(*l.e.Customer) == "" {
		return "Unknown"
	}
	return strings.TrimSpace(*l.e.Customer)
}

func costAgentID(l costLine) int {
	if l.e.TravelAgentID == nil {
		return 0
	}
	return *l.e.TravelAgentID
}

// costApply returns the lines that pass f, as a new slice.
func costApply(lines []costLine, f CostFilter) []costLine {
	src, emp, job := lowerSet(f.Sources), lowerSet(f.Employees), map[string]bool{}
	for _, v := range f.JobNumbers {
		job[strings.ToUpper(strings.TrimSpace(v))] = true
	}
	ctry, cust, ctype, paid := lowerSet(f.Countries), lowerSet(f.Customers), lowerSet(f.CostTypes), lowerSet(f.Paid)
	agents := map[int]bool{}
	for _, a := range f.TravelAgents {
		agents[a] = true
	}
	out := make([]costLine, 0, len(lines))
	for _, l := range lines {
		switch {
		case len(src) > 0 && !src[strings.ToLower(l.e.Source)]:
		case len(emp) > 0 && (l.traveller == nil || !emp[l.traveller.Email]):
		case len(job) > 0 && !job[l.job]:
		case len(ctry) > 0 && !ctry[strings.ToLower(costCountry(l))]:
		case len(agents) > 0 && !agents[costAgentID(l)]:
		case len(paid) > 0 && !paid[strings.ToLower(l.e.Paid)]:
		case len(cust) > 0 && !cust[strings.ToLower(costCustomer(l))]:
		case len(ctype) > 0 && !ctype[strings.ToLower(l.e.CostType)]:
		case f.Unlinked && l.job != "":
		default:
			out = append(out, l)
		}
	}
	return out
}

// ── Aggregation (pure) ──────────────────────────────────────────────────────

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func fptr(v float64) *float64 { return &v }

// costChange returns the previous value, change percent and thin flag. thin:
// no previous, or the value is over five times it; then there is no percent.
func costChange(value, previous float64, compare bool) (*float64, *float64, bool) {
	if !compare {
		return nil, nil, false
	}
	prev := fptr(dashboardRound(previous))
	if previous == 0 || value > 5*previous {
		return prev, nil, true
	}
	return prev, fptr(round1((value - previous) / previous * 100)), false
}

func costKpi(value, previous float64, compare bool) domain.CostKpi {
	p, c, thin := costChange(value, previous, compare)
	return domain.CostKpi{Value: dashboardRound(value), Previous: p, ChangePercent: c, Thin: thin}
}

// costStats are the figures of a set of lines.
type costStats struct {
	total, linked, unlinked float64
	trips, travellers       map[string]bool
}

func costStatsOf(lines []costLine) costStats {
	st := costStats{trips: map[string]bool{}, travellers: map[string]bool{}}
	for _, l := range lines {
		if l.job != "" {
			st.trips[l.job] = true
		}
		if l.traveller != nil && l.traveller.Email != "" {
			st.travellers[l.traveller.Email] = true
		}
		if !l.converted {
			continue
		}
		st.total += l.usd.USD
		if l.job != "" {
			st.linked += l.usd.USD
		} else {
			st.unlinked += l.usd.USD
		}
	}
	return st
}

func (st costStats) avg() float64 {
	if len(st.trips) == 0 {
		return 0
	}
	return st.linked / float64(len(st.trips))
}

type costKey struct{ key, label string }

// costGroupSums sums the USD of the converted lines per group.
func costGroupSums(lines []costLine, keyOf func(costLine) (costKey, bool)) (map[string]float64, map[string]costKey, map[string]map[string]bool) {
	sums := map[string]float64{}
	keys := map[string]costKey{}
	trips := map[string]map[string]bool{}
	for _, l := range lines {
		k, ok := keyOf(l)
		if !ok {
			continue
		}
		keys[k.key] = k
		if trips[k.key] == nil {
			trips[k.key] = map[string]bool{}
		}
		if l.job != "" {
			trips[k.key][l.job] = true
		}
		if l.converted {
			sums[k.key] += l.usd.USD
		} else if _, seen := sums[k.key]; !seen {
			sums[k.key] = 0
		}
	}
	return sums, keys, trips
}

// costBuckets builds the bars of one breakdown. top 0 keeps all; otherwise the
// top n by value and, when other is set, the rest as "Other" (key blank).
func costBuckets(cur, prev []costLine, compare bool, keyOf func(costLine) (costKey, bool), top int, other bool, extra func(*domain.CostBucket, string, map[string]map[string]bool, float64)) []domain.CostBucket {
	sums, keys, trips := costGroupSums(cur, keyOf)
	psums, _, _ := costGroupSums(prev, keyOf)
	type row struct {
		k costKey
		v float64
	}
	rows := make([]row, 0, len(sums))
	total := 0.0
	for k, v := range sums {
		rows = append(rows, row{keys[k], v})
		total += v
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].v != rows[j].v {
			return rows[i].v > rows[j].v
		}
		return rows[i].k.label < rows[j].k.label
	})
	build := func(k costKey, v, pv float64) domain.CostBucket {
		b := domain.CostBucket{Key: k.key, Label: k.label, Value: dashboardRound(v)}
		if total > 0 {
			b.Percent = dashboardRound(v / total * 100)
		}
		b.Previous, b.ChangePercent, b.Thin = costChange(v, pv, compare)
		if extra != nil {
			extra(&b, k.key, trips, v)
		}
		return b
	}
	out := []domain.CostBucket{}
	shown := map[string]bool{}
	for i, r := range rows {
		if top > 0 && i == top {
			break
		}
		shown[r.k.key] = true
		out = append(out, build(r.k, r.v, psums[r.k.key]))
	}
	if other && top > 0 && len(rows) > top {
		ov, op := 0.0, 0.0
		for _, r := range rows[top:] {
			ov += r.v
		}
		for k, v := range psums {
			if !shown[k] {
				op += v
			}
		}
		out = append(out, build(costKey{"", "Other"}, ov, op))
	}
	return out
}

func sourceKey(l costLine) (costKey, bool) {
	return costKey{l.e.Source, costSourceLabel[l.e.Source]}, true
}

func monthKey(l costLine) (costKey, bool) { return costKey{l.month, l.month}, true }

func countryKey(l costLine) (costKey, bool) {
	c := costCountry(l)
	return costKey{c, c}, true
}

func customerKey(l costLine) (costKey, bool) {
	c := costCustomer(l)
	return costKey{c, c}, l.e.Paid == domain.CostPaid
}

func agentKey(l costLine) (costKey, bool) {
	if l.job == "" {
		return costKey{}, false
	}
	id := costAgentID(l)
	name := "No travel agent"
	if l.e.TravelAgentName != nil && strings.TrimSpace(*l.e.TravelAgentName) != "" {
		name = strings.TrimSpace(*l.e.TravelAgentName)
	}
	return costKey{strconv.Itoa(id), name}, true
}

func costTypeKey(l costLine) (costKey, bool) {
	return costKey{l.e.CostType, l.e.CostType}, true
}

var costPaidLabel = map[string]string{domain.CostPaid: "Paid", domain.CostNonPaid: "Non-Paid", domain.CostUnknown: "Unknown"}

func paidKey(l costLine) (costKey, bool) {
	p := l.e.Paid
	if costPaidLabel[p] == "" {
		p = domain.CostUnknown
	}
	return costKey{p, costPaidLabel[p]}, true
}

func tripsExtra(b *domain.CostBucket, key string, trips map[string]map[string]bool, _ float64) {
	n := len(trips[key])
	b.Trips = &n
}

func agentExtra(b *domain.CostBucket, key string, trips map[string]map[string]bool, v float64) {
	tripsExtra(b, key, trips, v)
	avg := 0.0
	if n := len(trips[key]); n > 0 {
		avg = dashboardRound(v / float64(n))
	}
	b.AveragePerTrip = &avg
}

// costMonths lists the months of from..to.
func costMonths(from, to time.Time) []string {
	var out []string
	m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !m.After(to) {
		out = append(out, m.Format("2006-01"))
		m = m.AddDate(0, 1, 0)
	}
	return out
}

// costSummary builds the whole summary from the filtered lines of the period
// (cur), the filtered lines of the previous period (prev, nil without compare)
// and the facet lines (the period before the filters).
func costSummary(cur, prev, facetLines []costLine, allowed []string, p costPeriod, compare bool) domain.TravelCostSummary {
	out := domain.TravelCostSummary{Currency: "USD", RateRule: "monthEnd"}
	out.From, out.To = p.strings()
	cs, ps := costStatsOf(cur), costStatsOf(prev)
	out.Kpis = domain.CostKpis{
		Total:          costKpi(cs.total, ps.total, compare),
		Trips:          costKpi(float64(len(cs.trips)), float64(len(ps.trips)), compare),
		AveragePerTrip: costKpi(cs.avg(), ps.avg(), compare),
		Travellers:     costKpi(float64(len(cs.travellers)), float64(len(ps.travellers)), compare),
		UnlinkedSpend:  costKpi(cs.unlinked, ps.unlinked, compare),
	}

	// bySource: every allowed source, even at zero. A source with no current lines still has last year's spend to compare with.
	srcBuckets := costBuckets(cur, prev, compare, sourceKey, 0, false, nil)
	prevBySource, _, _ := costGroupSums(prev, sourceKey)
	have := map[string]domain.CostBucket{}
	for _, b := range srcBuckets {
		have[b.Key] = b
	}
	out.BySource = []domain.CostBucket{}
	for _, src := range costSourceOrder {
		if !contains(allowed, src) {
			continue
		}
		b, ok := have[src]
		if !ok {
			b = domain.CostBucket{Key: src, Label: costSourceLabel[src]}
			b.Previous, b.ChangePercent, b.Thin = costChange(0, prevBySource[src], compare)
		}
		out.BySource = append(out.BySource, b)
	}

	// byMonth: every month, one series per allowed source.
	by := map[string]map[string]float64{}
	for _, l := range cur {
		if !l.converted {
			continue
		}
		if by[l.month] == nil {
			by[l.month] = map[string]float64{}
		}
		by[l.month][costSourceLabel[l.e.Source]] += l.usd.USD
	}
	out.ByMonth = []domain.DashboardSeriesPoint{}
	for _, m := range costMonths(p.from, p.to) {
		vals := map[string]float64{}
		for _, src := range allowed {
			vals[costSourceLabel[src]] = dashboardRound(by[m][costSourceLabel[src]])
		}
		out.ByMonth = append(out.ByMonth, domain.DashboardSeriesPoint{Month: m, Values: vals})
	}

	out.ByCountry = costBuckets(cur, prev, compare, countryKey, costTopN, true, nil)
	out.ByCustomer = costBuckets(cur, prev, compare, customerKey, costTopN, false, tripsExtra)
	out.ByTravelAgent = costBuckets(cur, prev, compare, agentKey, 0, false, agentExtra)
	out.ByCostType = costBuckets(cur, prev, compare, costTypeKey, 0, false, func(b *domain.CostBucket, key string, _ map[string]map[string]bool, _ float64) {
		cat := key != "Card" && key != "Claim"
		b.Categorised = &cat
	})

	// byPaid: three fixed bars.
	paidBuckets := costBuckets(cur, prev, compare, paidKey, 0, false, nil)
	prevByPaid, _, _ := costGroupSums(prev, paidKey)
	hp := map[string]domain.CostBucket{}
	for _, b := range paidBuckets {
		hp[b.Key] = b
	}
	out.ByPaid = []domain.CostBucket{}
	for _, k := range []string{domain.CostPaid, domain.CostNonPaid, domain.CostUnknown} {
		b, ok := hp[k]
		if !ok {
			b = domain.CostBucket{Key: k, Label: costPaidLabel[k]}
			b.Previous, b.ChangePercent, b.Thin = costChange(0, prevByPaid[k], compare)
		}
		out.ByPaid = append(out.ByPaid, b)
	}
	if paid, non := hp[domain.CostPaid].Value, hp[domain.CostNonPaid].Value; paid+non > 0 {
		out.PaidSharePercent = fptr(round1(paid / (paid + non) * 100))
	}

	out.NotConverted = costNotConverted(cur)

	out.Facets = costFacets(facetLines)
	out.FailedSources = []string{}
	return out
}

// costNotConverted lists the spend of lines with no rate, by source and currency.
func costNotConverted(lines []costLine) []domain.CostNotConverted {
	type nk struct{ src, cur string }
	sums, counts := map[nk]float64{}, map[nk]int{}
	for _, l := range lines {
		if !l.converted {
			k := nk{l.e.Source, strings.ToUpper(strings.TrimSpace(l.e.OriginalCurrency))}
			sums[k] += l.e.OriginalAmount
			counts[k]++
		}
	}
	out := []domain.CostNotConverted{}
	for k, v := range sums {
		out = append(out, domain.CostNotConverted{Source: k.src, Currency: k.cur, Amount: dashboardRound(v), Lines: counts[k]})
	}
	order := func(s string) int {
		for i, o := range costSourceOrder {
			if o == s {
				return i
			}
		}
		return len(costSourceOrder)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Source != b.Source {
			return order(a.Source) < order(b.Source)
		}
		return a.Currency < b.Currency
	})
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func costFacets(lines []costLine) domain.CostFacets {
	f := domain.CostFacets{Countries: []string{}, TravelAgents: []domain.CostTravelAgent{}, Customers: []string{}, CostTypes: []string{}}
	ctry, cust, types := map[string]bool{}, map[string]bool{}, map[string]bool{}
	agents := map[int]string{}
	for _, l := range lines {
		ctry[costCountry(l)] = true
		types[l.e.CostType] = true
		if l.e.Customer != nil && strings.TrimSpace(*l.e.Customer) != "" {
			cust[strings.TrimSpace(*l.e.Customer)] = true
		}
		if l.e.TravelAgentID != nil && l.e.TravelAgentName != nil {
			agents[*l.e.TravelAgentID] = *l.e.TravelAgentName
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	f.Countries, f.Customers, f.CostTypes = keys(ctry), keys(cust), keys(types)
	for id, name := range agents {
		f.TravelAgents = append(f.TravelAgents, domain.CostTravelAgent{ID: id, Name: name})
	}
	sort.Slice(f.TravelAgents, func(i, j int) bool { return f.TravelAgents[i].Name < f.TravelAgents[j].Name })
	return f
}

// ── Lines and groups ───────────────────────────────────────────────

func costLineView(l costLine) domain.CostLine {
	e := l.e
	out := domain.CostLine{
		ID: e.ID, Source: e.Source, Reference: e.Reference, JobNumber: e.JobNumber, Traveller: l.traveller,
		Date: e.Date, OriginalAmount: e.OriginalAmount, OriginalCurrency: e.OriginalCurrency, CostType: e.CostType,
		SourceType: e.SourceType, Country: e.Country, Customer: e.Customer, Paid: e.Paid,
		EngagementID: e.EngagementID, AllocationType: e.AllocationType,
	}
	if out.Paid == "" {
		out.Paid = domain.CostUnknown
	}
	if e.TravelAgentID != nil {
		out.TravelAgent = &domain.CostTravelAgent{ID: *e.TravelAgentID}
		if e.TravelAgentName != nil {
			out.TravelAgent.Name = *e.TravelAgentName
		}
	}
	if l.converted {
		out.USDAmount = fptr(dashboardRound(l.usd.USD))
		out.Rate = fptr(l.usd.Rate)
		src := l.usd.Source
		out.RateSource = &src
		if l.usd.RateDate != "" {
			d := l.usd.RateDate
			out.RateDate = &d
		}
	}
	return out
}

func costSortLines(lines []costLine, sortBy string) {
	less := func(i, j int) bool {
		return lines[i].e.Date < lines[j].e.Date || (lines[i].e.Date == lines[j].e.Date && lines[i].e.ID < lines[j].e.ID)
	}
	switch sortBy {
	case "date":
	case "amount", "-amount":
		sort.SliceStable(lines, func(i, j int) bool {
			a, b := lines[i], lines[j]
			if a.converted != b.converted {
				return a.converted
			}
			if a.usd.USD != b.usd.USD {
				if sortBy == "amount" {
					return a.usd.USD < b.usd.USD
				}
				return a.usd.USD > b.usd.USD
			}
			return less(i, j)
		})
		return
	default: // -date
		sort.SliceStable(lines, func(i, j int) bool { return less(j, i) })
		return
	}
	sort.SliceStable(lines, less)
}

var costGroupKeys = map[string]func(costLine) (costKey, bool){
	"employee": func(l costLine) (costKey, bool) {
		if l.traveller == nil {
			return costKey{"", "Unknown"}, true
		}
		label := l.traveller.Email
		if l.traveller.Name != nil && *l.traveller.Name != "" {
			label = *l.traveller.Name
		}
		return costKey{l.traveller.Email, label}, true
	},
	"jobNumber": func(l costLine) (costKey, bool) {
		if l.job == "" {
			return costKey{"", "No job number"}, true
		}
		return costKey{l.job, l.job}, true
	},
	"country":  countryKey,
	"customer": func(l costLine) (costKey, bool) { c := costCustomer(l); return costKey{c, c}, true },
	"travelAgent": func(l costLine) (costKey, bool) {
		k, _ := agentKey(l)
		if l.job == "" {
			k = costKey{strconv.Itoa(costAgentID(l)), "No travel agent"}
		}
		return k, true
	},
	"source":   sourceKey,
	"costType": costTypeKey,
	"month":    monthKey,
}

// costMonthLastYear is the same month a year earlier: 2026-10 becomes 2025-10.
func costMonthLastYear(month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	return t.AddDate(-1, 0, 0).Format("2006-01")
}

func costGroups(cur, prev []costLine, compare bool, groupBy, sortBy string) ([]domain.CostGroup, error) {
	keyOf, ok := costGroupKeys[groupBy]
	if !ok {
		return nil, &apierror.ValidationError{Msg: "groupBy must be employee, jobNumber, country, customer, travelAgent, source, costType or month."}
	}
	type acc struct {
		k          costKey
		lines      int
		total      float64
		trips      map[string]bool
		travellers map[string]bool
	}
	groups := map[string]*acc{}
	for _, l := range cur {
		k, _ := keyOf(l)
		g := groups[k.key]
		if g == nil {
			g = &acc{k: k, trips: map[string]bool{}, travellers: map[string]bool{}}
			groups[k.key] = g
		}
		g.lines++
		if l.job != "" {
			g.trips[l.job] = true
		}
		if l.traveller != nil {
			g.travellers[l.traveller.Email] = true
		}
		if l.converted {
			g.total += l.usd.USD
		}
	}
	psums, _, _ := costGroupSums(prev, func(l costLine) (costKey, bool) { return keyOf(l) })
	out := make([]domain.CostGroup, 0, len(groups))
	for key, g := range groups {
		cg := domain.CostGroup{Key: key, Label: g.k.label, Lines: g.lines, Trips: len(g.trips), Travellers: len(g.travellers), Total: dashboardRound(g.total)}
		// The previous period is the same dates a year earlier, so a month is compared with the same month last year.
		prevKey := key
		if groupBy == "month" {
			prevKey = costMonthLastYear(key)
		}
		cg.Previous, cg.ChangePercent, cg.Thin = costChange(g.total, psums[prevKey], compare)
		out = append(out, cg)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch sortBy {
		case "amount":
			if a.Total != b.Total {
				return a.Total < b.Total
			}
		case "date", "-date":
			if a.Key != b.Key {
				if sortBy == "date" {
					return a.Key < b.Key
				}
				return a.Key > b.Key
			}
		default:
			if a.Total != b.Total {
				return a.Total > b.Total
			}
		}
		return a.Label < b.Label
	})
	return out, nil
}

// ── Service methods ─────────────────────────────────────────────────────────

// costPrepared is a query resolved for a caller: the trimmed, filtered lines
// of both periods.
type costPrepared struct {
	period     costPeriod
	prevPeriod *costPeriod
	allowed    []string
	selected   []string // allowed narrowed by the source filter: the sources the figures really use
	cur, prev  []costLine
	facetLines []costLine
	set        *costSet
	prevSet    *costSet
}

// prevFailed is the selected sources that could not be read for the comparison period.
func (c *costPrepared) prevFailed() []string {
	if c.prevSet == nil {
		return nil
	}
	var out []string
	for _, f := range c.prevSet.failed {
		if contains(c.selected, f) {
			out = append(out, f)
		}
	}
	return out
}

// currentFailed is the selected sources that could not be read for the current period.
func (c *costPrepared) currentFailed() []string {
	var out []string
	for _, f := range c.set.failed {
		if contains(c.selected, f) {
			out = append(out, f)
		}
	}
	return out
}

// comparable is whether both periods are complete enough to compare. A source
// missing from either side would read as a rise or a fall in spend.
func (c *costPrepared) comparable() bool {
	if c.prevSet == nil {
		return true
	}
	return len(c.prevFailed()) == 0 && !c.prevSet.truncated && !c.prevSet.ratesPending && len(c.currentFailed()) == 0 && !c.set.truncated
}

// incompleteText names what is missing from the data in one short phrase, or is empty when nothing is.
func (c *costPrepared) incompleteText() string {
	var parts []string
	for _, f := range c.set.failed {
		if contains(c.allowed, f) {
			parts = append(parts, costSourceLabel[f]+" missing")
		}
	}
	if c.set.truncated {
		parts = append(parts, "20,000 line cap reached")
	}
	if c.set.ratesPending {
		parts = append(parts, "Exchange rates still loading")
	}
	if len(c.prevFailed()) > 0 || (c.prevSet != nil && (c.prevSet.truncated || c.prevSet.ratesPending)) {
		parts = append(parts, "Last year incomplete")
	}
	return strings.Join(parts, ", ")
}

// costSelected narrows the allowed sources to the ones the filter asks for, or keeps them all when it asks for none.
func costSelected(allowed, want []string) []string {
	if len(want) == 0 {
		return allowed
	}
	var out []string
	for _, a := range allowed {
		for _, w := range want {
			if strings.EqualFold(a, w) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

func (s *dashboardService) prepare(ctx context.Context, p domain.Perms, q CostQuery) (*costPrepared, error) {
	allowed, ok := costSources(p)
	if !ok {
		return nil, errCostForbidden
	}
	cur, prevP, err := s.resolve(q)
	if err != nil {
		return nil, err
	}
	curSet, prevSet, err := s.loadSets(ctx, cur, prevP)
	if err != nil {
		return nil, err
	}
	trimmed := costTrim(curSet.lines, allowed)
	out := &costPrepared{period: cur, prevPeriod: prevP, allowed: allowed, selected: costSelected(allowed, q.Filter.Sources),
		set: curSet, prevSet: prevSet, facetLines: trimmed,
		cur: costApply(trimmed, q.Filter)}
	if prevSet != nil {
		out.prev = costApply(costTrim(prevSet.lines, allowed), q.Filter)
	}
	return out, nil
}

func (s *dashboardService) TravelCosts(ctx context.Context, p domain.Perms, q CostQuery) (*domain.TravelCostSummary, error) {
	pr, err := s.prepare(ctx, p, q)
	if err != nil {
		return nil, err
	}
	// Last year's figures with a source missing would read as a fall in spend, so then only the current figures are given.
	sum := costSummary(pr.cur, pr.prev, pr.facetLines, pr.allowed, pr.period, q.Compare && pr.comparable())
	sum.ComparisonFailedSources = pr.prevFailed()
	if pr.prevSet != nil {
		sum.ComparisonTruncated = pr.prevSet.truncated
	}
	if pr.prevPeriod != nil {
		f, t := pr.prevPeriod.strings()
		sum.Compare = &domain.CostPeriod{From: f, To: t}
	}
	sum.DataAsOf = pr.set.asOf.Format(time.RFC3339)
	sum.Truncated = pr.set.truncated
	for _, l := range pr.cur {
		if l.pending {
			sum.RatesPending = true
			break
		}
	}
	for _, f := range pr.set.failed {
		if contains(pr.allowed, f) {
			sum.FailedSources = append(sum.FailedSources, f)
		}
	}
	return &sum, nil
}

func (s *dashboardService) CostTransactions(ctx context.Context, p domain.Perms, q CostQuery, t CostTransactionsQuery) (any, error) {
	switch t.Sort {
	case "", "date", "-date", "amount", "-amount":
	default:
		return nil, &apierror.ValidationError{Msg: "sort must be date, -date, amount or -amount."}
	}
	if t.Limit <= 0 {
		t.Limit = 25
	}
	if t.Limit > 100 || t.Offset < 0 {
		return nil, &apierror.ValidationError{Msg: "limit is 1 to 100 and offset is not negative."}
	}
	if t.GroupBy != "" {
		if _, ok := costGroupKeys[t.GroupBy]; !ok {
			return nil, &apierror.ValidationError{Msg: "groupBy must be employee, jobNumber, country, customer, travelAgent, source, costType or month."}
		}
	}
	// Only grouped rows carry last year's figures, so a plain page must not
	// depend on, or fail with, the prior-period read.
	if t.GroupBy == "" {
		q.Compare = false
	}
	pr, err := s.prepare(ctx, p, q)
	if err != nil {
		return nil, err
	}
	asOf := pr.set.asOf.Format(time.RFC3339)
	if t.GroupBy != "" {
		groups, err := costGroups(pr.cur, pr.prev, q.Compare && pr.comparable(), t.GroupBy, t.Sort)
		if err != nil {
			return nil, err
		}
		total := len(groups)
		groups = costPage(groups, t.Limit, t.Offset)
		return domain.CostGroupPage{Groups: groups, Total: total, DataAsOf: asOf}, nil
	}
	lines := append([]costLine(nil), pr.cur...)
	costSortLines(lines, t.Sort)
	sum := 0.0
	for _, l := range lines {
		if l.converted {
			sum += l.usd.USD
		}
	}
	page := domain.CostLinePage{Items: []domain.CostLine{}, Total: len(lines), TotalUSD: dashboardRound(sum), DataAsOf: asOf}
	for _, l := range costPage(lines, t.Limit, t.Offset) {
		page.Items = append(page.Items, costLineView(l))
	}
	return page, nil
}

func costPage[T any](list []T, limit, offset int) []T {
	if offset >= len(list) {
		return []T{}
	}
	end := min(offset+limit, len(list))
	return list[offset:end]
}

// ── CSV ─────────────────────────────────────────────────────────────────────
//
// Built for a spreadsheet user. Summary-type files start with a two-column
// header block (a short label in column A, its value in column B), a blank
// row, then titled sections, each with a real header row. The transactions
// file is a plain table: header row first, nothing above it. Numbers are
// plain (no thousands separators or symbols): money has 2 decimals, counts
// are whole numbers, percentages have 1 decimal; the unit is in the header.
// Dates are YYYY-MM-DD and months YYYY-MM. No cell holds a paragraph.

// costRateRuleText is the value of the "How USD is calculated" row.
const costRateRuleText = "USD amounts as recorded. Others at NetSuite month-end rate (today's rate for this month)"

func money(v float64) string { return strconv.FormatFloat(dashboardRound(v), 'f', 2, 64) }

func optMoney(v *float64) string {
	if v == nil {
		return ""
	}
	return money(*v)
}

func optPct(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 1, 64)
}

func costFilterText(f CostFilter) string {
	var parts []string
	add := func(name string, vals []string, label func(string) string) {
		if len(vals) == 0 {
			return
		}
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = label(v)
		}
		parts = append(parts, name+"="+strings.Join(out, "|"))
	}
	same := func(s string) string { return s }
	add("Source", f.Sources, func(s string) string { return costSourceLabel[s] })
	add("Employee", f.Employees, same)
	add("Job Number", f.JobNumbers, same)
	add("Destination Country", f.Countries, same)
	agents := make([]string, len(f.TravelAgents))
	for i, a := range f.TravelAgents {
		agents[i] = strconv.Itoa(a)
	}
	add("Travel Agent", agents, same)
	add("Engagement", f.Paid, func(s string) string { return costPaidLabel[s] })
	add("Customer", f.Customers, same)
	add("Cost Type", f.CostTypes, same)
	if f.Unlinked {
		parts = append(parts, "Unlinked=Yes")
	}
	if len(parts) == 0 {
		return "None"
	}
	return strings.Join(parts, ", ")
}

// costHeaderBlock is the two-column block at the top of a summary-type file.
func costHeaderBlock(title string, pr *costPrepared, q CostQuery) [][]string {
	f, t := pr.period.strings()
	rows := [][]string{
		{"Report", title},
		{"Date range", f + " to " + t},
	}
	if pr.prevPeriod != nil && q.Compare {
		pf, pt := pr.prevPeriod.strings()
		rows = append(rows, []string{"Compared with", pf + " to " + pt})
	} else {
		rows = append(rows, []string{"Compared with", "Not compared"})
	}
	rows = append(rows,
		[]string{"Filters", costFilterText(q.Filter)},
		[]string{"Amounts in", "USD"},
		[]string{"How USD is calculated", costRateRuleText},
	)
	// A total with a source missing must not look complete to whoever reads the file.
	if text := pr.incompleteText(); text != "" {
		rows = append(rows, []string{"Incomplete data", text})
	}
	return append(rows, []string{"Data as of", pr.set.asOf.Format("2006-01-02 15:04") + " UTC"})
}

var costSectionTitle = map[string]string{
	"kpis": "Key Figures", "bySource": "Spend by Source", "byMonth": "Spend by Month", "byCountry": "Spend by Destination Country",
	"byCustomer": "Spend by Paid Customer", "byTravelAgent": "Travel Agent Comparison", "byCostType": "Spend by Travel App Cost Type",
	"byPaid": "Paid vs Non-Paid Engagements",
}

// costSectionLabel names the first column of a section.
var costSectionLabel = map[string]string{
	"bySource": "Source", "byCountry": "Country", "byCustomer": "Customer", "byTravelAgent": "Travel Agent",
	"byCostType": "Cost Type", "byPaid": "Engagement Type",
}

// costSectionFile is the part of the file name of each export.
var costSectionFile = map[string]string{
	"summary": "Summary", "kpis": "Key_Figures", "bySource": "By_Source", "byMonth": "By_Month", "byCountry": "By_Country",
	"byCustomer": "By_Customer", "byTravelAgent": "By_Travel_Agent", "byCostType": "By_Cost_Type", "byPaid": "Paid_vs_Non_Paid",
	"transactions": "Transactions",
}

var costSectionOrder = []string{"kpis", "bySource", "byMonth", "byCountry", "byCustomer", "byTravelAgent", "byCostType", "byPaid"}

// costCompareHead is the comparison columns, left out when there is no comparison.
func costCompareHead(compare bool, unit string) []string {
	if !compare {
		return nil
	}
	return []string{"Same Period Last Year" + unit, "Change vs Last Year (%)"}
}

func costBucketRows(label string, bs []domain.CostBucket, compare bool, extra ...string) [][]string {
	head := append([]string{label, "This Period (USD)"}, costCompareHead(compare, " (USD)")...)
	head = append(head, extra...)
	rows := [][]string{head}
	for _, b := range bs {
		r := []string{b.Label, money(b.Value)}
		if compare {
			r = append(r, optMoney(b.Previous), optPct(b.ChangePercent))
		}
		for _, e := range extra {
			switch e {
			case "Trips":
				if b.Trips != nil {
					r = append(r, strconv.Itoa(*b.Trips))
				} else {
					r = append(r, "")
				}
			case "Average per Trip (USD)":
				r = append(r, optMoney(b.AveragePerTrip))
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func costSectionRows(section string, sum domain.TravelCostSummary, allowed []string, compare bool) [][]string {
	switch section {
	case "kpis":
		k := sum.Kpis
		row := func(name string, v domain.CostKpi, isMoney bool) []string {
			fm := func(x float64) string { return strconv.FormatFloat(x, 'f', 0, 64) }
			if isMoney {
				fm = money
			}
			r := []string{name, fm(v.Value)}
			if compare {
				prev := ""
				if v.Previous != nil {
					prev = fm(*v.Previous)
				}
				r = append(r, prev, optPct(v.ChangePercent))
			}
			return r
		}
		return [][]string{append([]string{"Figure", "This Period"}, costCompareHead(compare, "")...),
			row("Total Spend (USD)", k.Total, true), row("Trips", k.Trips, false), row("Average per Trip (USD)", k.AveragePerTrip, true),
			row("Travellers", k.Travellers, false), row("Unlinked Spend (USD)", k.UnlinkedSpend, true)}
	case "bySource", "byCountry", "byCostType", "byPaid":
		return costBucketRows(costSectionLabel[section], map[string][]domain.CostBucket{
			"bySource": sum.BySource, "byCountry": sum.ByCountry, "byCostType": sum.ByCostType, "byPaid": sum.ByPaid}[section], compare)
	case "byMonth":
		head := []string{"Month"}
		for _, s := range allowed {
			head = append(head, costSourceLabel[s]+" (USD)")
		}
		head = append(head, "Total (USD)")
		rows := [][]string{head}
		for _, m := range sum.ByMonth {
			r := []string{m.Month}
			var cents int64
			for _, s := range allowed {
				v := dashboardRound(m.Values[costSourceLabel[s]])
				cents += int64(math.Round(v * 100))
				r = append(r, money(v))
			}
			rows = append(rows, append(r, strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)))
		}
		return rows
	case "byCustomer":
		return costBucketRows(costSectionLabel[section], sum.ByCustomer, compare, "Trips")
	case "byTravelAgent":
		return costBucketRows(costSectionLabel[section], sum.ByTravelAgent, compare, "Trips", "Average per Trip (USD)")
	}
	return nil
}

func costFileName(name, from, to string) string {
	return fmt.Sprintf("Travel_Costs_%s_%s_%s.csv", name, from, to)
}

func (s *dashboardService) TravelCostsCSV(ctx context.Context, p domain.Perms, q CostQuery, section string) (*CostCSV, error) {
	if section == "" {
		section = "summary"
	}
	if section != "summary" && costSectionTitle[section] == "" {
		return nil, &apierror.ValidationError{Msg: "section must be summary, kpis, bySource, byMonth, byCountry, byCustomer, byTravelAgent, byCostType or byPaid."}
	}
	pr, err := s.prepare(ctx, p, q)
	if err != nil {
		return nil, err
	}
	compare := q.Compare && pr.comparable()
	sum := costSummary(pr.cur, pr.prev, pr.facetLines, pr.allowed, pr.period, compare)
	f, t := pr.period.strings()
	secs := costSectionOrder
	title := "Travel Costs Summary"
	if section != "summary" {
		secs = []string{section}
		title = "Travel Costs: " + costSectionTitle[section]
	}
	// The header block must not claim a comparison that is not in the file.
	hq := q
	hq.Compare = compare && section != "byMonth" // the monthly table has no comparison columns
	rows := costHeaderBlock(title, pr, hq)
	for _, sec := range secs {
		rows = append(rows, []string{}, []string{costSectionTitle[sec]})
		rows = append(rows, costSectionRows(sec, sum, pr.allowed, compare)...)
	}
	return &CostCSV{Filename: costFileName(costSectionFile[section], f, t), Rows: rows}, nil
}

// costHowConverted says in words how a line's USD amount came about.
func costHowConverted(src *string) string {
	if src == nil {
		return "Not converted (no rate)"
	}
	switch *src {
	case rateSourceRecorded:
		return "Recorded in USD"
	case rateSourceCard:
		return "Billed in USD"
	case rateSourceMonthEnd:
		return "NetSuite month-end rate"
	case rateSourceToday:
		return "NetSuite rate of the day (current month)"
	}
	return *src
}

// costTransactionsHead is the one header row of the transactions file.
var costTransactionsHead = []string{"Date", "Source", "Reference", "Travel Job Number", "Employee", "Email", "Destination Country", "Customer",
	"Paid / Non-Paid", "Cost Type", "Amount", "Currency", "Amount (USD)", "Rate Used", "Rate Date", "How Converted"}

// CostTransactionsCSV is a plain table, header row first, one row per line, newest first.
func (s *dashboardService) CostTransactionsCSV(ctx context.Context, p domain.Perms, q CostQuery) (*CostCSV, error) {
	q.Compare = false // the file is current-period lines only
	pr, err := s.prepare(ctx, p, q)
	if err != nil {
		return nil, err
	}
	lines := append([]costLine(nil), pr.cur...)
	costSortLines(lines, "-date")
	rows := [][]string{costTransactionsHead}
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for _, l := range lines {
		v := costLineView(l)
		name, email := "", ""
		if v.Traveller != nil {
			name, email = strings.TrimSpace(str(v.Traveller.Name)), v.Traveller.Email
		}
		rate := ""
		if v.Rate != nil {
			rate = strconv.FormatFloat(*v.Rate, 'f', -1, 64)
		}
		ref := str(v.Reference)
		if ref == "" {
			ref = v.ID
		}
		rows = append(rows, []string{v.Date, costSourceLabel[v.Source], ref, str(v.JobNumber), name, email, strings.TrimSpace(str(v.Country)),
			str(v.Customer), costPaidLabel[v.Paid], v.CostType, money(v.OriginalAmount), strings.ToUpper(v.OriginalCurrency),
			optMoney(v.USDAmount), rate, str(v.RateDate), costHowConverted(v.RateSource)})
	}
	f, t := pr.period.strings()
	return &CostCSV{Filename: costFileName(costSectionFile["transactions"], f, t), Rows: rows}, nil
}
