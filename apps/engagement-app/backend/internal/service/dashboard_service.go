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
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/apierror"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/clients/engagemententity"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/clients/hrentity"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/config"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/domain"
)

// DashboardService builds the Dashboard, Allocations Today and the Me
// overview card. Every section is built only from data the caller can
// already see with their legacy privileges; work queues ("Waiting on You")
// use the privileges without the super admin groups (Perms.AllocOwn /
// TravelOwn).
type DashboardService interface {
	// Dashboard builds the caller's sections. sections asks for exactly
	// those (unknown keys are a validation error, sections the caller may
	// not see are left out); nil means every section behind a widget that
	// is on by default.
	Dashboard(ctx context.Context, p domain.Perms, sections []string) (domain.Dashboard, error)
	AllocationsToday(ctx context.Context, p domain.Perms, date string, businessUnitID int) (domain.DashboardAllocationsToday, error)
	MeOverview(ctx context.Context, p domain.Perms) (domain.MeOverview, error)

	// Travel Costs (finance groups and the Travel Super Admin; 403 otherwise).
	TravelCosts(ctx context.Context, p domain.Perms, q CostQuery) (*domain.TravelCostSummary, error)
	TravelCostsCSV(ctx context.Context, p domain.Perms, q CostQuery, section string) (*CostCSV, error)
	// CostTransactions returns a domain.CostLinePage, or a domain.CostGroupPage with groupBy.
	CostTransactions(ctx context.Context, p domain.Perms, q CostQuery, t CostTransactionsQuery) (any, error)
	CostTransactionsCSV(ctx context.Context, p domain.Perms, q CostQuery) (*CostCSV, error)

	// JobCostSummary is the cost so far of one travel request, for whoever may
	// open that request (no per-source split for a travel agent).
	JobCostSummary(ctx context.Context, p domain.Perms, jobNumber string) (*domain.JobCostSummary, error)
}

// DashboardOption configures a DashboardService.
type DashboardOption func(*dashboardService)

// WithNoValidVisaID sets the visa_status id of "No Valid Visa Available".
func WithNoValidVisaID(id int) DashboardOption {
	return func(s *dashboardService) { s.noValidVisaID = id }
}

// WithCacheTTL sets how long this pod keeps the shared dashboard data (cost
// line sets and entity aggregates). Zero or negative keeps nothing. The default
// is config.DefaultDashboardCacheTTL.
func WithCacheTTL(ttl time.Duration) DashboardOption {
	return func(s *dashboardService) { s.cacheTTL = ttl }
}

type dashboardService struct {
	entity engagemententity.DashboardAPI
	people *travelPeople
	rates  *dashboardRates
	now    func() time.Time

	cacheTTL time.Duration
	// costCache holds cost line sets per date range; aggCache the entity
	// aggregates per query. Both hold only data that is the same for every
	// caller; privileges and scope are applied after the read.
	costCache     *costSetCache
	aggCache      *loadCache[any]
	noValidVisaID int
}

// InvalidateCache drops everything this pod has cached for the dashboard. It
// is called after any write, so the numbers a user just changed are not shown
// stale in this pod. Other replicas keep theirs until the TTL ends.
func (s *dashboardService) InvalidateCache() {
	s.costCache.clear()
	s.aggCache.clear()
}

// CacheInvalidator is implemented by the dashboard service.
type CacheInvalidator interface{ InvalidateCache() }

// cachedAgg reads one entity aggregate through the shared cache. key must name
// every input of load (the entity call and all its parameters) and nothing
// about the caller, so one caller's answer is never another's. The value is
// shared and must be treated as read-only.
func cachedAgg[T any](ctx context.Context, s *dashboardService, key string, load func(context.Context) (*T, error)) (*T, error) {
	v, err := s.aggCache.get(ctx, key, func() (any, error) {
		// The load is shared with every caller that joined this key, so one caller leaving must not cancel it.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), costEntityBudget)
		defer cancel()
		return load(lctx)
	})
	if err != nil {
		return nil, err
	}
	return v.(*T), nil
}

// aggKey is the cache key of an entity call: its name and parameters.
func aggKey(name string, params any) string {
	b, err := json.Marshal(params)
	if err != nil {
		return name + "|" + fmt.Sprintf("%+v", params)
	}
	return name + "|" + string(b)
}

// NewDashboardService constructs a DashboardService. hr may be nil (no
// traveller names); currency may be nil (every non-USD amount is then listed
// as not converted); now may be nil (time.Now).
func NewDashboardService(entity engagemententity.DashboardAPI, hr hrentity.Client, currency FinanceCurrencyConverter, now func() time.Time, opts ...DashboardOption) DashboardService {
	if now == nil {
		now = time.Now
	}
	s := &dashboardService{entity: entity, now: now, rates: newDashboardRates(currency, now),
		cacheTTL: config.DefaultDashboardCacheTTL, noValidVisaID: 333}
	for _, o := range opts {
		o(s)
	}
	s.costCache = newCostSetCache(costSetCacheSize, s.cacheTTL, nil)
	s.aggCache = newLoadCache[any](dashboardAggCacheSize, s.cacheTTL, dashboardAggMaxLoads, nil)
	if hr != nil {
		s.people = newTravelPeople(hr)
	}
	return s
}

const dashboardDateLayout = "2006-01-02"

// Labels of the Travel Cost series and types (prototype views-dashboard).
const (
	dashboardSeriesInvoices = "Travel Invoices"
	dashboardSeriesPerDiems = "Per Diems"
	dashboardSeriesCards    = "Credit Card Expenses"
	dashboardSeriesClaims   = "Expense Claims"
)

// dashboardCardGroups are the travel finance groups that see card spend.
var dashboardCardGroups = []string{
	"app-travel-approver-finance",
	"app-travel-approver-perdiem-finance",
	"app-travel-user-finance",
}

// today is the current UTC day (the entity's sessions are UTC).
func (s *dashboardService) today() time.Time {
	y, m, d := s.now().UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ── Privilege rules ─────────────────────────────────────────────────────────

// dashboardKind is the engagement kind of an engagement type id.
func dashboardKind(typeID int) string {
	switch typeID {
	case 1:
		return "internal"
	case 5:
		return "marketing"
	}
	return "customer"
}

var dashboardEngagementTypeIDs = []int{1, 2, 3, 4, 5, 6, 7}

// dashboardCanAllocateType is the legacy per-type allocate rule: 6, 16 or 21
// to open the Allocate page, then 32 (post sale 4, 6), 33 (pre sale 3, 7), 6
// (retired Paid 2) or 16 (internal 1, marketing 5).
func dashboardCanAllocateType(p domain.PrivilegeSet, typeID int) bool {
	if !p.Any(6, 16, 21) {
		return false
	}
	switch typeID {
	case 4, 6:
		return p.Has(32)
	case 3, 7:
		return p.Has(33)
	case 2:
		return p.Has(6)
	case 1, 5:
		return p.Has(16)
	}
	return false
}

// dashboardSeesEngagementType is engRole: customer types need 3, 6, 32 or
// 33; internal engagements and marketing events need 13 or 16.
func dashboardSeesEngagementType(p domain.PrivilegeSet, typeID int) bool {
	if dashboardKind(typeID) == "customer" {
		return p.Any(3, 6, 32, 33)
	}
	return p.Any(13, 16)
}

// dashboardClearanceTypes are the engagement types on which the caller may
// set an allocation's status (canEditStatus: the allocate rule for customer
// types, 20 for internal and marketing) and see the engagement; not the
// retired Paid type (2).
func dashboardClearanceTypes(p domain.PrivilegeSet) []int {
	var out []int
	for _, t := range dashboardEngagementTypeIDs {
		if t == 2 {
			continue
		}
		edit := dashboardCanAllocateType(p, t)
		if dashboardKind(t) != "customer" {
			edit = p.Has(20)
		}
		if edit && dashboardSeesEngagementType(p, t) {
			out = append(out, t)
		}
	}
	return out
}

// dashboardHasCardRole reports whether the caller is in a travel finance group.
func dashboardHasCardRole(p domain.Perms) bool {
	for _, g := range dashboardCardGroups {
		if domain.HasRole(p.TravelRoles, g) {
			return true
		}
	}
	return false
}

// ── Concurrency ─────────────────────────────────────────────────────────────

// dashboardGroup runs entity calls in parallel and records the sections of
// the ones that fail, so the others are still returned.
type dashboardGroup struct {
	wg          sync.WaitGroup
	mu          sync.Mutex
	failed      []string
	err         error
	calls       int
	failedCalls int
}

func (g *dashboardGroup) Go(ctx context.Context, sections []string, fn func() error) {
	g.calls++
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(); err != nil {
			slog.ErrorContext(ctx, "dashboard section failed", "sections", sections, "err", err)
			g.mu.Lock()
			g.failed = append(g.failed, sections...)
			g.failedCalls++
			if g.err == nil {
				g.err = err
			}
			g.mu.Unlock()
		}
	}()
}

// Wait returns the failed sections, sorted, or the error when every call failed.
func (g *dashboardGroup) Wait() ([]string, error) {
	g.wg.Wait()
	if g.calls > 0 && g.err != nil && len(g.failed) > 0 && g.allFailed() {
		return nil, g.err
	}
	sort.Strings(g.failed)
	if g.failed == nil {
		g.failed = []string{}
	}
	return g.failed, nil
}

func (g *dashboardGroup) allFailed() bool { return g.failedCalls == g.calls }

// ── Dashboard ───────────────────────────────────────────────────────────────

func (s *dashboardService) Dashboard(ctx context.Context, p domain.Perms, sections []string) (domain.Dashboard, error) {
	asked := map[string]bool{}
	for _, k := range sections {
		known := false
		for _, v := range DashboardSectionKeys {
			known = known || v == k
		}
		if !known {
			return domain.Dashboard{}, &apierror.ValidationError{Msg: "sections: unknown section " + strconv.Quote(k) + "."}
		}
		asked[k] = true
	}
	today := s.today()
	todayStr := today.Format(dashboardDateLayout)
	if p.IsAgent() {
		agent, err := s.agent(ctx, p, todayStr, today)
		if err != nil {
			return domain.Dashboard{}, err
		}
		var people []*domain.Person
		for i := range agent.Upcoming {
			people = append(people, &agent.Upcoming[i].Traveller)
		}
		s.people.fillNames(ctx, people...)
		return domain.Dashboard{Date: todayStr, Agent: agent, FailedSections: []string{}, Widgets: []domain.DashboardWidget{}}, nil
	}

	out := domain.Dashboard{Date: todayStr}
	out.Widgets = dashboardWidgets(p)
	// The sections to build: the ones asked for, else those behind a widget
	// that is on by default; either way only those the caller may see.
	visibleSections := dashboardSectionSet(out.Widgets, false)
	want := map[string]bool{}
	if sections == nil {
		want = dashboardSectionSet(out.Widgets, true)
	} else {
		for k := range asked {
			want[k] = visibleSections[k]
		}
	}
	g := &dashboardGroup{}
	gctx := ctx
	tsa := domain.HasRole(p.TravelRoles, domain.TravelSuperAdminGroup)

	// Waiting on You.
	plan := dashboardPlanQueues(p)
	if want[sectionWaitingOnYou] && plan.any() {
		g.Go(ctx, []string{"waitingOnYou"}, func() error {
			q, err := s.entity.DashboardQueues(gctx, plan.filter)
			if err != nil {
				return err
			}
			out.WaitingOnYou = plan.cards(q)
			return nil
		})
	}

	// Travel: the Travel Team (Travel 1) and the Travel Super Admin. Travel
	// Cost (deprecated: Travel Costs has its own endpoints) is built only
	// when no sections are asked for, for the finance groups and the Travel
	// Super Admin.
	tf := engagemententity.DashboardTravelFilter{Today: todayStr, Trips: want[sectionTravel] && (p.Travel.Has(1) || tsa)}
	card := dashboardHasCardRole(p) && p.TravelOwn.Any(39, 41)
	if sections == nil && (card || tsa) {
		if tsa {
			tf.CostInvoices, tf.CostPayments, tf.CostCards, tf.CostClaims = true, true, true, true
		} else {
			tf.CostInvoices = p.TravelOwn.Any(39, 30)
			tf.CostPayments = p.TravelOwn.Any(41, 42)
			tf.CostCards, tf.CostClaims = true, true
		}
		card = true
	} else {
		card = false
	}
	cost := tf.CostInvoices || tf.CostPayments || tf.CostCards || tf.CostClaims
	if tf.Trips || cost {
		var secs []string
		if tf.Trips {
			secs = append(secs, "travel")
		}
		if cost {
			secs = append(secs, "travelCost")
		}
		g.Go(ctx, secs, func() error {
			t, err := cachedAgg(gctx, s, aggKey("travel", tf), func(c context.Context) (*engagemententity.DashboardTravel, error) {
				return s.entity.DashboardTravel(c, tf)
			})
			if err != nil {
				return err
			}
			if tf.Trips && t.Trips != nil {
				out.Travel = dashboardTravel(t.Trips, today)
			}
			if cost {
				out.TravelCost = dashboardTravelCost(gctx, t, today, card, s.rates)
			}
			return nil
		})
	}

	// Engagements (3 or 13), Available Consultants (9, 19 or 21),
	// Allocations Over Time (1 and an allocation, report, approval or
	// allocate privilege).
	ef := engagemententity.DashboardEngagementsFilter{Today: todayStr}
	showEngs := want[sectionEngagements]
	if showEngs {
		for _, t := range dashboardEngagementTypeIDs {
			if dashboardSeesEngagementType(p.Alloc, t) {
				ef.EngagementTypes = append(ef.EngagementTypes, t)
			}
		}
	}
	ef.Bench = want[sectionAvailableConsultants]
	ef.Trend = want[sectionAllocationsOverTime]
	if showEngs || ef.Bench || ef.Trend {
		var secs []string
		if showEngs {
			secs = append(secs, sectionEngagements)
		}
		if ef.Bench {
			secs = append(secs, sectionAvailableConsultants)
		}
		if ef.Trend {
			secs = append(secs, sectionAllocationsOverTime)
		}
		g.Go(ctx, secs, func() error {
			e, err := cachedAgg(gctx, s, aggKey("engagements", ef), func(c context.Context) (*engagemententity.DashboardEngagements, error) {
				return s.entity.DashboardEngagements(c, ef)
			})
			if err != nil {
				return err
			}
			if showEngs {
				out.Engagements = dashboardEngagements(e)
			}
			if ef.Bench {
				out.AvailableConsultants = dashboardBench(e.Bench, today)
			}
			if ef.Trend && e.Trend != nil {
				out.AllocationsOverTime = dashboardTrend(e.Trend, today)
			}
			return nil
		})
	}

	s.roleSections(ctx, g, p, want, newWidgetSet(out.Widgets), todayStr, today, &out)

	failed, err := g.Wait()
	if err != nil {
		return domain.Dashboard{}, err
	}
	out.FailedSections = failed
	var people []*domain.Person
	if out.Travel != nil {
		for i := range out.Travel.Upcoming {
			people = append(people, &out.Travel.Upcoming[i].Traveller)
		}
	}
	people = append(people, dashboardRolePeople(&out)...)
	s.people.fillNames(ctx, people...)
	if mc := out.MarketingEvents; mc != nil && mc.CampaignCoverage == nil && newWidgetSet(out.Widgets).has("ev-campaign") {
		// The campaign_id column is absent here: ev-campaign is not offered.
		kept := out.Widgets[:0]
		for _, w := range out.Widgets {
			if w.ID != "ev-campaign" {
				kept = append(kept, w)
			}
		}
		out.Widgets = kept
	}
	return out, nil
}

// ── Own allocations and trips (shared with the Me overview) ─────────────────

// dashboardEngagementTitle is the prototype's engName: "Account – Title"
// for a customer engagement whose title does not start with the account's
// first word; otherwise the title.
func dashboardEngagementTitle(id string, typeID *int, account, title *string) string {
	t := strings.TrimSpace(dashboardStr(title))
	a := strings.TrimSpace(dashboardStr(account))
	if typeID != nil && dashboardKind(*typeID) == "customer" && a != "" {
		if t == "" {
			return a
		}
		first := strings.ToLower(strings.Fields(a)[0])
		if !strings.HasPrefix(strings.ToLower(t), first) {
			return a + " – " + t
		}
	}
	if t == "" {
		return id
	}
	return t
}

func dashboardStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dashboardAllocation(a engagemententity.DashboardOwnAllocation, current bool, visible map[int]bool) domain.DashboardAllocationSummary {
	return domain.DashboardAllocationSummary{
		ID: a.ID, EngagementID: a.EngagementID, CanOpen: a.EngagementTypeID != nil && visible[*a.EngagementTypeID],
		EngagementTitle: dashboardEngagementTitle(a.EngagementID, a.EngagementTypeID, a.Account, a.Title),
		StartDate:       a.StartDate, EndDate: a.EndDate, ClearanceStatus: a.ClearanceStatus, Current: current,
	}
}

// dashboardSplitAllocations returns the current allocation (the first that
// covers today) and the future ones, in start order. The entity returns
// only allocations ending today or later.
func dashboardSplitAllocations(list []engagemententity.DashboardOwnAllocation, today string, visible map[int]bool) (*domain.DashboardAllocationSummary, []domain.DashboardAllocationSummary) {
	var current *domain.DashboardAllocationSummary
	next := []domain.DashboardAllocationSummary{}
	for _, a := range list {
		switch {
		case a.StartDate > today:
			next = append(next, dashboardAllocation(a, false, visible))
		case current == nil:
			c := dashboardAllocation(a, true, visible)
			current = &c
		}
	}
	return current, next
}

func dashboardTrips(list []engagemententity.DashboardTrip) []domain.DashboardTrip {
	out := make([]domain.DashboardTrip, 0, len(list))
	for _, t := range list {
		out = append(out, domain.DashboardTrip{
			JobNumber: t.JobNumber, Status: t.Status, Traveller: domain.Person{Email: t.TravellerEmail},
			City: t.City, Country: t.Country, StartDate: t.StartDate, EndDate: t.EndDate, Agency: t.Agency,
		})
	}
	return out
}

// ── Waiting on You ──────────────────────────────────────────────────────────

// dashboardQueuePlan is which cards apply to the caller and the entity
// counts they need. Every gate uses the own privileges (no super admin).
type dashboardQueuePlan struct {
	filter                                    engagemententity.DashboardQueuesFilter
	pending, clearance, newTravel             bool
	approvePo, approve, push, pay, failed     bool
	failedInvoices, failedPayments, internals bool
}

func dashboardPlanQueues(p domain.Perms) dashboardQueuePlan {
	ao, to := p.AllocOwn, p.TravelOwn
	var pl dashboardQueuePlan
	// Pending Approval only for people who can approve (legacy: 28 plus an
	// engagement update privilege, for both queues), over the queues they see.
	if ao.Has(28) && ao.Any(4, 14, 7, 17, 5) {
		if ao.Has(26) {
			pl.filter.PendingApprovalTypes = append(pl.filter.PendingApprovalTypes, 1)
			pl.internals = true
		}
		if ao.Has(29) {
			pl.filter.PendingApprovalTypes = append(pl.filter.PendingApprovalTypes, 5)
		}
	}
	pl.pending = len(pl.filter.PendingApprovalTypes) > 0
	pl.filter.ClearanceTypes = dashboardClearanceTypes(ao)
	pl.clearance = len(pl.filter.ClearanceTypes) > 0
	pl.newTravel = to.Has(1)
	pl.filter.TravelRequests = pl.newTravel
	pl.approvePo = to.Has(32)
	// The finance step only for users without HR approval.
	pl.approve = to.Has(33) && !to.Has(32)
	pl.push = to.Has(34)
	pl.pay = to.Has(40)
	pl.failedInvoices, pl.failedPayments = to.Has(34), to.Has(40)
	pl.failed = pl.failedInvoices || pl.failedPayments
	pl.filter.Invoices = pl.approvePo || pl.approve || pl.push
	pl.filter.Payments = pl.pay
	return pl
}

func (pl dashboardQueuePlan) any() bool {
	return pl.pending || pl.clearance || pl.newTravel || pl.approvePo || pl.approve || pl.push || pl.pay || pl.failed
}

func (pl dashboardQueuePlan) cards(q *engagemententity.DashboardQueues) []domain.DashboardCountCard {
	out := []domain.DashboardCountCard{}
	// route is the One WSO2 screen under /finance/engagement-app.
	add := func(key, label string, n int, hot bool, route string) {
		out = append(out, domain.DashboardCountCard{Key: key, Label: label, Count: n, Highlighted: hot,
			List: domain.DashboardListLink{Path: "/finance/engagement-app/" + route}})
	}
	if pl.pending {
		n := 0
		for _, t := range pl.filter.PendingApprovalTypes {
			n += q.PendingApproval[t]
		}
		route := "approvals/marketing"
		if pl.internals {
			route = "approvals/internal"
		}
		add("pendingApproval", "Pending Approval", n, true, route)
	}
	if pl.clearance {
		// The list opens on the types counted, so the tile and list agree.
		const st = "Ready for Clearance"
		route := "allocations/engagement?clearanceStatus=" + url.PathEscape(st)
		for _, t := range pl.filter.ClearanceTypes {
			route += "&engagementTypeId=" + strconv.Itoa(t)
		}
		add("readyForClearance", st, q.Clearance[st], true, route)
	}
	if pl.newTravel {
		add("newTravelRequests", "New Travel Requests", q.NewTravelRequests, true, "travel-requests/new")
	}
	if pl.approvePo {
		add("approve-po", "Invoices to Approve (PO)", q.InvoicesPending, true, "invoices/po")
	}
	if pl.approve {
		add("approve", "Invoices to Approve", q.InvoicesAcceptedByTravel, true, "invoices/finance")
	}
	if pl.push {
		add("push", "Invoices to Push", q.InvoicesToPush, false, "invoices/push")
	}
	if pl.pay {
		add("pay", "Per Diem Payments to Push", q.PaymentsToPush, false, "invoices/payments-push")
	}
	if pl.failed {
		n := 0
		if pl.failedInvoices {
			n += q.InvoicesFailed
		}
		if pl.failedPayments {
			n += q.PaymentsFailed
		}
		add("failed", "NS Sync Failures", n, false, "invoices/sync-failures")
	}
	return out
}

// ── Charts ──────────────────────────────────────────────────────────────────

// dashboardMonths returns the 12 months ending with today's, as YYYY-MM.
func dashboardMonths(today time.Time) []string {
	first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]string, 12)
	for i := 0; i < 12; i++ {
		out[i] = first.AddDate(0, i-11, 0).Format("2006-01")
	}
	return out
}

// dashboardRound rounds to 2 decimals (the legacy getRoundOffNumber).
func dashboardRound(v float64) float64 { return math.Round(v*100) / 100 }

type dashboardRow struct {
	label string
	value float64
	id    *int
	color *string
}

// dashboardBuckets sorts rows by value (descending, then label), keeps the
// top n with the rest summed as "Other" (n 0 keeps all), and sets each
// bucket's percentage of the total.
func dashboardBuckets(rows []dashboardRow, n int) []domain.DashboardBucket {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].value != rows[j].value {
			return rows[i].value > rows[j].value
		}
		return rows[i].label < rows[j].label
	})
	if n > 0 && len(rows) > n {
		other := dashboardRow{label: "Other"}
		for _, r := range rows[n-1:] {
			other.value += r.value
		}
		rows = append(rows[:n-1:n-1], other)
	}
	total := 0.0
	for _, r := range rows {
		total += r.value
	}
	out := make([]domain.DashboardBucket, 0, len(rows))
	for _, r := range rows {
		b := domain.DashboardBucket{Label: r.label, Value: dashboardRound(r.value), ID: r.id, Color: r.color}
		if total > 0 {
			b.Percent = dashboardRound(r.value / total * 100)
		}
		out = append(out, b)
	}
	return out
}

func dashboardCountBuckets(counts []engagemententity.DashboardCount, n int) []domain.DashboardBucket {
	rows := make([]dashboardRow, 0, len(counts))
	for _, c := range counts {
		rows = append(rows, dashboardRow{label: c.Label, value: float64(c.Count)})
	}
	return dashboardBuckets(rows, n)
}

// dashboardMonthSeries spreads month counts over the 12 months under one
// series name, zero-filled.
func dashboardMonthSeries(counts []engagemententity.DashboardCount, months []string, name string) []domain.DashboardSeriesPoint {
	by := map[string]int{}
	for _, c := range counts {
		by[c.Label] += c.Count
	}
	out := make([]domain.DashboardSeriesPoint, 0, len(months))
	for _, m := range months {
		out = append(out, domain.DashboardSeriesPoint{Month: m, Values: map[string]float64{name: float64(by[m])}})
	}
	return out
}

// ── Travel ──────────────────────────────────────────────────────────────────

func dashboardTravel(t *engagemententity.DashboardTravelTrips, today time.Time) *domain.DashboardTravel {
	return &domain.DashboardTravel{
		InProgress: t.InProgress, StartingIn30Days: t.StartingIn30Days, CompletedThisMonth: t.CompletedThisMonth,
		Last12Months: t.Last12Months, Upcoming: dashboardTrips(t.Upcoming), UpcomingTotal: t.UpcomingTotal,
		TopDestinations: dashboardCountBuckets(t.ByCountry, 6),
		ByMonth:         dashboardMonthSeries(t.ByMonth, dashboardMonths(today), "Trips"),
		ByAgency:        dashboardCountBuckets(t.ByAgency, 0),
	}
}

// dashboardTravelCost sums the cost lines in USD. Each line is converted
// from its original currency at NetSuite's rate on the last day of its month
// (today's rate for the current month); USD lines, and card lines billed in
// USD, are used as recorded. A line with no rate (blank currency, unknown
// rate, rate service down or not configured) is reported in NotConverted and
// left out of every total.
func dashboardTravelCost(ctx context.Context, t *engagemententity.DashboardTravel, today time.Time, card bool, rates *dashboardRates) *domain.DashboardTravelCost {
	months := dashboardMonths(today)
	inWindow := map[string]bool{}
	for _, m := range months {
		inWindow[m] = true
	}
	var pairs []ratePair
	for _, l := range t.Cost {
		if (l.Source == engagemententity.DashboardCostCard || l.Source == engagemententity.DashboardCostClaim) && !card {
			continue
		}
		if inWindow[l.Month] && usdNeedsRate(l.Currency, l.CCCurrency) {
			pairs = append(pairs, ratePair{strings.ToUpper(strings.TrimSpace(l.Currency)), dashboardRateDate(l.Month, today)})
		}
	}
	rs, _ := rates.resolve(ctx, pairs)
	series := map[string]map[string]float64{}
	byType := map[string]float64{}
	type key struct{ source, currency string }
	unconverted := map[key]float64{}
	var inv, pd, cc, ec float64
	for _, l := range t.Cost {
		if (l.Source == engagemententity.DashboardCostCard || l.Source == engagemententity.DashboardCostClaim) && !card {
			continue
		}
		if !inWindow[l.Month] {
			continue
		}
		conv, ok := rs.usd(l.Currency, l.Amount, l.CCCurrency, l.CCAmount, l.Month, today)
		if !ok {
			unconverted[key{l.Source, strings.ToUpper(strings.TrimSpace(l.Currency))}] += l.Amount
			continue
		}
		amount := conv.USD
		var name, typ string
		switch l.Source {
		case engagemententity.DashboardCostInvoice:
			inv += amount
			name, typ = dashboardSeriesInvoices, l.CostType
			if typ == "" {
				typ = "Other"
			}
		case engagemententity.DashboardCostPerDiem:
			pd += amount
			name, typ = dashboardSeriesPerDiems, dashboardSeriesPerDiems
		case engagemententity.DashboardCostCard:
			cc += amount
			name, typ = dashboardSeriesCards, dashboardSeriesCards
		case engagemententity.DashboardCostClaim:
			ec += amount
			name, typ = dashboardSeriesClaims, dashboardSeriesClaims
		default:
			continue
		}
		if series[l.Month] == nil {
			series[l.Month] = map[string]float64{}
		}
		series[l.Month][name] += amount
		byType[typ] += amount
	}

	out := &domain.DashboardTravelCost{
		Currency: "USD", RateRule: "monthEnd", From: t.From, To: t.To,
		Total: dashboardRound(inv + pd + cc + ec), TravelInvoices: dashboardRound(inv), PerDiems: dashboardRound(pd),
		ByMonth: make([]domain.DashboardSeriesPoint, 0, len(months)), NotConverted: []domain.DashboardUnconverted{},
	}
	if card {
		v, e := dashboardRound(cc), dashboardRound(ec)
		out.CreditCardExpenses, out.ExpenseClaims = &v, &e
	}
	for i, m := range months {
		vals := map[string]float64{dashboardSeriesInvoices: 0, dashboardSeriesPerDiems: 0}
		if card {
			vals[dashboardSeriesCards], vals[dashboardSeriesClaims] = 0, 0
		}
		sum := 0.0
		for k, v := range series[m] {
			vals[k] = dashboardRound(v)
			sum += v
		}
		out.ByMonth = append(out.ByMonth, domain.DashboardSeriesPoint{Month: m, Values: vals})
		switch i {
		case len(months) - 2:
			out.LastMonth = dashboardRound(sum)
		case len(months) - 1:
			out.CurrentMonth = dashboardRound(sum)
		}
	}
	rows := make([]dashboardRow, 0, len(byType))
	for k, v := range byType {
		rows = append(rows, dashboardRow{label: k, value: v})
	}
	out.ByType = dashboardBuckets(rows, 0)
	for k, v := range unconverted {
		out.NotConverted = append(out.NotConverted, domain.DashboardUnconverted{Source: k.source, Currency: k.currency, Amount: dashboardRound(v)})
	}
	sort.Slice(out.NotConverted, func(i, j int) bool {
		a, b := out.NotConverted[i], out.NotConverted[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Currency < b.Currency
	})
	return out
}

// ── Engagements, Available Consultants, Allocations Over Time ──────────────

func dashboardEngagements(e *engagemententity.DashboardEngagements) *domain.DashboardEngagements {
	out := &domain.DashboardEngagements{StartingIn30Days: []domain.DashboardEngagement{}, StartingIn30DaysTotal: e.StartingSoonTotal}
	for _, x := range e.StartingSoon {
		typeID := 0
		if x.EngagementTypeID != nil {
			typeID = *x.EngagementTypeID
		}
		kind := dashboardKind(typeID)
		var account *string
		if kind == "customer" {
			account = x.Account
		}
		out.StartingIn30Days = append(out.StartingIn30Days, domain.DashboardEngagement{
			ID: x.ID, Kind: kind, EngagementTypeID: typeID, EngagementTypeName: x.EngagementTypeName,
			Account: account, Title: dashboardStr(x.Title), AllocationType: x.AllocationType,
			EngagementCode: x.EngagementCode, City: x.City, Country: x.Country, StartDate: x.StartDate,
			EndDate: x.EndDate, RequestStatus: x.RequestStatus, EngagementStatus: x.EngagementStatus,
			Consultants: x.Consultants,
		})
	}
	return out
}

func dashboardBench(rows []engagemententity.DashboardBenchRow, today time.Time) *domain.DashboardAvailableConsultants {
	out := &domain.DashboardAvailableConsultants{
		From: today.Format(dashboardDateLayout), To: today.AddDate(0, 0, 13).Format(dashboardDateLayout),
	}
	free := make([]dashboardRow, 0, len(rows))
	for _, r := range rows {
		out.Consultants += r.Consultants
		out.Allocated += r.Allocated
		if n := r.Consultants - r.Allocated; n > 0 {
			free = append(free, dashboardRow{label: r.BusinessUnit, value: float64(n)})
		}
	}
	out.Available = out.Consultants - out.Allocated
	out.ByBusinessUnit = dashboardBuckets(free, 0)
	return out
}

func dashboardTrend(t *engagemententity.DashboardTrend, today time.Time) *domain.DashboardAllocationsOverTime {
	out := &domain.DashboardAllocationsOverTime{
		ByMonth:        dashboardMonthSeries(t.ByMonth, dashboardMonths(today), "Allocations"),
		ByBusinessUnit: dashboardCountBuckets(t.ByBusinessUnit, 0),
	}
	best := -1.0
	for i := range out.ByMonth {
		if v := out.ByMonth[i].Values["Allocations"]; v > best {
			best = v
			p := out.ByMonth[i]
			out.BusiestMonth = &p
		}
	}
	return out
}

// ── Travel agent ────────────────────────────────────────────────────────────

func (s *dashboardService) agent(ctx context.Context, p domain.Perms, todayStr string, today time.Time) (*domain.DashboardAgent, error) {
	// Keyed by the agency: an agent only ever reads their own agency's data.
	a, err := cachedAgg(ctx, s, aggKey("agency", []any{p.Agency.ID, todayStr}), func(c context.Context) (*engagemententity.DashboardAgency, error) {
		return s.entity.DashboardAgency(c, p.Agency.ID, todayStr)
	})
	if err != nil {
		return nil, err
	}
	out := &domain.DashboardAgent{
		Agency: p.Agency.Name, InProgress: a.InProgress, StartingIn7Days: a.StartingIn7Days,
		CompletedWithoutInvoice: a.CompletedWithoutInvoice,
		UpcomingTotal:           a.UpcomingTotal, Upcoming: []domain.DashboardAgentTrip{},
		InvoicesByStatus: dashboardCountBuckets(a.InvoicesByStatus, 0),
		TripsByMonth:     dashboardMonthSeries(a.TripsByMonth, dashboardMonths(today), "Trips"),
	}
	for _, c := range a.InvoicesByStatus {
		if c.Label == "Rejected by travel" || c.Label == "Rejected by finance" {
			out.InvoicesRejected += c.Count
		}
	}
	for _, t := range a.Upcoming {
		out.Upcoming = append(out.Upcoming, domain.DashboardAgentTrip{
			JobNumber: t.JobNumber, Status: t.Status, Traveller: domain.Person{Email: t.TravellerEmail},
			City: t.City, Country: t.Country, StartDate: t.StartDate, EndDate: t.EndDate,
		})
	}
	return out, nil
}

// ── Allocations Today ───────────────────────────────────────────────────────

// AllocationsToday runs the legacy Allocations dashboard for date (default:
// today, UTC, the business date). Every day, today included, is counted by
// the same rule and sent to the entity explicitly; the response carries the
// date it was computed for. A later date is refused, as the legacy screen did.
func (s *dashboardService) AllocationsToday(ctx context.Context, _ domain.Perms, date string, businessUnitID int) (domain.DashboardAllocationsToday, error) {
	today := s.today().Format(dashboardDateLayout)
	if date == "" {
		date = today
	}
	if _, err := time.Parse(dashboardDateLayout, date); err != nil {
		return domain.DashboardAllocationsToday{}, &apierror.ValidationError{Msg: "date must be a date (YYYY-MM-DD)."}
	}
	if date > today {
		return domain.DashboardAllocationsToday{}, &apierror.ValidationError{Msg: "The date cannot be in the future."}
	}
	if businessUnitID < 0 {
		return domain.DashboardAllocationsToday{}, &apierror.ValidationError{Msg: "businessUnitId must be a positive whole number."}
	}
	r, err := cachedAgg(ctx, s, aggKey("allocationsToday", []any{date, businessUnitID}), func(c context.Context) (*engagemententity.DashboardAllocationsToday, error) {
		return s.entity.DashboardAllocationsToday(c, date, businessUnitID)
	})
	if err != nil {
		return domain.DashboardAllocationsToday{}, err
	}
	out := domain.DashboardAllocationsToday{Date: date, Consultants: r.Consultants}
	if businessUnitID > 0 {
		out.BusinessUnitID = &businessUnitID
	}
	et := make([]dashboardRow, 0, len(r.ByEngagementType))
	for _, c := range r.ByEngagementType {
		id := c.EngagementTypeID
		et = append(et, dashboardRow{label: c.EngagementType, value: float64(c.Count), id: &id})
		out.Allocations += c.Count
	}
	out.ByEngagementType = dashboardBuckets(et, 8)
	typeRows := func(list []engagemententity.DashboardAllocationTypeCount, sum *int) []domain.DashboardBucket {
		rows := make([]dashboardRow, 0, len(list))
		for _, c := range list {
			rows = append(rows, dashboardRow{label: c.AllocationType, value: float64(c.Count), color: c.Color})
			*sum += c.Count
		}
		return dashboardBuckets(rows, 8)
	}
	out.PaidByAllocationType = typeRows(r.PaidByAllocationType, &out.Paid)
	out.NonPaidByAllocationType = typeRows(r.NonPaidByAllocationType, &out.NonPaid)
	return out, nil
}

// ── Me overview ─────────────────────────────────────────────────────────────

// MeOverview is the Allocations & Trips card: the current and next own
// active allocations (at most 3) and own trips not Cancelled ending today or
// later (at most 3).
func (s *dashboardService) MeOverview(ctx context.Context, p domain.Perms) (domain.MeOverview, error) {
	out := domain.MeOverview{UpcomingAllocations: []domain.DashboardAllocationSummary{}, UpcomingTrips: []domain.DashboardTrip{}}
	allocs, trips := p.Alloc.Has(1), p.Travel.Any(49, 36)
	if !allocs && !trips {
		return out, nil
	}
	today := s.today().Format(dashboardDateLayout)
	mw, err := s.entity.DashboardMyWork(ctx, engagemententity.DashboardMyWorkFilter{
		Email: p.Email, Today: today, Trips: trips, TripLimit: 3,
	})
	if err != nil {
		return domain.MeOverview{}, err
	}
	if allocs {
		current, next := dashboardSplitAllocations(mw.Allocations, today, allocListTypes(p))
		if current != nil {
			out.UpcomingAllocations = append(out.UpcomingAllocations, *current)
		}
		for _, a := range next {
			if len(out.UpcomingAllocations) == 3 {
				break
			}
			out.UpcomingAllocations = append(out.UpcomingAllocations, a)
		}
	}
	if trips {
		out.UpcomingTrips = dashboardTrips(mw.Trips)
		// The visas are an extra on the card: when their lookup fails the allocations and trips already read are still returned.
		if visas, verr := s.tripVisas(ctx, p.Email, today); verr != nil {
			slog.WarnContext(ctx, "me overview trip visas failed", "err", verr)
		} else {
			out.TripVisas = visas
		}
	}
	return out, nil
}
