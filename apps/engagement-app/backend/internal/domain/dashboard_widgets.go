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

package domain

// Dashboard widgets (Release 2): the widget list, the role sections of GET
// /dashboard, the Travel Costs endpoints and the Me visas.

// DashboardWidget is one widget the caller may see. DefaultOn is whether it
// is shown until the person changes it.
type DashboardWidget struct {
	ID        string `json:"id"`
	DefaultOn bool   `json:"defaultOn"`
}

// ── Role sections of GET /dashboard ─────────────────────────────────────────

// DashboardPoQueue is the invoices waiting for PO approval.
type DashboardPoQueue struct {
	Count      int               `json:"count"`
	OldestDays int               `json:"oldestDays"`
	ByAge      []DashboardBucket `json:"byAge,omitzero"`
}

// DashboardFrequentTraveller is a traveller with their trip count.
type DashboardFrequentTraveller struct {
	Traveller Person `json:"traveller"`
	Trips     int    `json:"trips"`
}

// DashboardAway is who is travelling this week.
type DashboardAway struct {
	Travellers int `json:"travellers"`
	Countries  int `json:"countries"`
}

// DashboardCount is a bare count.
type DashboardCount struct {
	Count int `json:"count"`
}

// DashboardTravelTeam is the Travel Team section. Each part is present only
// when the caller may see the widget it feeds (tt-glance: awayThisWeek,
// endedNotCompleted and poQueue count and oldestDays; tt-po-age: poQueue
// byAge; tt-frequent; tt-agent-workload).
type DashboardTravelTeam struct {
	AwayThisWeek       *DashboardAway               `json:"awayThisWeek,omitempty"`
	EndedNotCompleted  *DashboardCount              `json:"endedNotCompleted,omitempty"`
	PoQueue            *DashboardPoQueue            `json:"poQueue,omitempty"`
	FrequentTravellers []DashboardFrequentTraveller `json:"frequentTravellers,omitzero"`
	AgentWorkload      []DashboardBucket            `json:"agentWorkload,omitzero"`
}

// DashboardEngagementList is a short list of engagements with the total.
type DashboardEngagementList struct {
	Total int                   `json:"total"`
	Items []DashboardEngagement `json:"items"`
}

// DashboardStartingAllocation is an allocation starting soon.
type DashboardStartingAllocation struct {
	AllocationID    string `json:"allocationId"`
	Consultant      Person `json:"consultant"`
	EngagementID    string `json:"engagementId"`
	EngagementTitle string `json:"engagementTitle"`
	StartDate       string `json:"startDate"`
	EndDate         string `json:"endDate"`
	TravelRequired  bool   `json:"travelRequired"`
}

// DashboardMostAllocated is a consultant with their allocated days.
type DashboardMostAllocated struct {
	Consultant Person `json:"consultant"`
	Days       int    `json:"days"`
}

// DashboardStartingList is the allocations starting soon, with the total.
type DashboardStartingList struct {
	Total int                           `json:"total"`
	Items []DashboardStartingAllocation `json:"items"`
}

// DashboardTopCustomers is the top customers (or countries) by allocated days.
type DashboardTopCustomers struct {
	By    string            `json:"by"`
	Items []DashboardBucket `json:"items"`
}

// DashboardEngagementManager is the Engagement Manager section. Each part is
// present only when the caller may see its widget.
type DashboardEngagementManager struct {
	WithoutConsultants *DashboardEngagementList `json:"withoutConsultants,omitempty"`
	StartingIn14Days   *DashboardStartingList   `json:"startingIn14Days,omitempty"`
	MostAllocated      []DashboardMostAllocated `json:"mostAllocated,omitzero"`
	TopCustomers       *DashboardTopCustomers   `json:"topCustomers,omitempty"`
}

// DashboardAccountManager is the Account Manager section. Each part is
// present only when the caller may see its widget.
type DashboardAccountManager struct {
	StartedWithoutConsultants *DashboardEngagementList `json:"startedWithoutConsultants,omitempty"`
	ByStatus                  []DashboardBucket        `json:"byStatus,omitzero"`
	ByMonth                   []DashboardSeriesPoint   `json:"byMonth,omitzero"`
}

// DashboardCampaignCreator is the campaign coverage of one creator.
type DashboardCampaignCreator struct {
	Creator      Person  `json:"creator"`
	Total        int     `json:"total"`
	WithCampaign int     `json:"withCampaign"`
	Percent      float64 `json:"percent"`
}

// DashboardCampaignCoverage is the Campaign ID coverage of recent events.
type DashboardCampaignCoverage struct {
	Total        int                        `json:"total"`
	WithCampaign int                        `json:"withCampaign"`
	Percent      float64                    `json:"percent"`
	ByCreator    []DashboardCampaignCreator `json:"byCreator"`
}

// DashboardMarketingEvents is the Marketing Events section.
type DashboardMarketingEvents struct {
	ByMonth               []DashboardSeriesPoint     `json:"byMonth,omitzero"`
	ByRegion              []DashboardBucket          `json:"byRegion,omitzero"`
	FundingByBusinessUnit []DashboardBucket          `json:"fundingByBusinessUnit,omitzero"`
	CampaignCoverage      *DashboardCampaignCoverage `json:"campaignCoverage,omitempty"`
}

// ── Me: visas for upcoming trips ────────────────────────────────────────────

// TripMyVisa is the caller's own visa that covers a trip.
type TripMyVisa struct {
	Type    string `json:"type"`
	EndDate string `json:"endDate"`
}

// TripVisa is an upcoming trip with its visa state.
type TripVisa struct {
	JobNumber string      `json:"jobNumber"`
	Country   string      `json:"country"`
	City      *string     `json:"city"`
	StartDate string      `json:"startDate"`
	EndDate   string      `json:"endDate"`
	TripVisa  *string     `json:"tripVisa"`
	MyVisa    *TripMyVisa `json:"myVisa"`
	Waiting   bool        `json:"waiting"`
}

// ── Travel Costs ────────────────────────────────────────────────────────────

// Cost line sources and paid values.
const (
	CostSourceInvoice = "invoice"
	CostSourcePerDiem = "perDiem"
	CostSourceCard    = "card"
	CostSourceClaim   = "claim"
)

// Paid values of a cost line.
const (
	CostPaid    = "paid"
	CostNonPaid = "nonPaid"
	CostUnknown = "unknown"
)

// CostKpi is a KPI with the same figure a year earlier.
type CostKpi struct {
	Value         float64  `json:"value"`
	Previous      *float64 `json:"previous"`
	ChangePercent *float64 `json:"changePercent"`
	Thin          bool     `json:"thin"`
}

// CostBucket is a bar of a Travel Costs breakdown.
type CostBucket struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Value          float64  `json:"value"`
	Percent        float64  `json:"percent"`
	Previous       *float64 `json:"previous"`
	ChangePercent  *float64 `json:"changePercent"`
	Thin           bool     `json:"thin"`
	Trips          *int     `json:"trips,omitempty"`
	AveragePerTrip *float64 `json:"averagePerTrip,omitempty"`
	Categorised    *bool    `json:"categorised,omitempty"`
}

// CostNotConverted is spend with no NetSuite rate, left out of the USD figures.
type CostNotConverted struct {
	Source   string  `json:"source"`
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
	Lines    int     `json:"lines"`
}

// CostPeriod is a date range.
type CostPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// CostTravelAgent is a travel agent of a cost line or a facet.
type CostTravelAgent struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// CostFacets are the filter lists, built before the filters.
type CostFacets struct {
	Countries    []string          `json:"countries"`
	TravelAgents []CostTravelAgent `json:"travelAgents"`
	Customers    []string          `json:"customers"`
	CostTypes    []string          `json:"costTypes"`
}

// CostKpis are the Travel Costs KPIs.
type CostKpis struct {
	Total          CostKpi `json:"total"`
	Trips          CostKpi `json:"trips"`
	AveragePerTrip CostKpi `json:"averagePerTrip"`
	Travellers     CostKpi `json:"travellers"`
	UnlinkedSpend  CostKpi `json:"unlinkedSpend"`
}

// TravelCostSummary is GET /dashboard/travel-costs.
type TravelCostSummary struct {
	Currency         string                 `json:"currency"`
	RateRule         string                 `json:"rateRule"`
	From             string                 `json:"from"`
	To               string                 `json:"to"`
	Compare          *CostPeriod            `json:"compare"`
	DataAsOf         string                 `json:"dataAsOf"`
	Kpis             CostKpis               `json:"kpis"`
	BySource         []CostBucket           `json:"bySource"`
	ByMonth          []DashboardSeriesPoint `json:"byMonth"`
	ByCountry        []CostBucket           `json:"byCountry"`
	ByCustomer       []CostBucket           `json:"byCustomer"`
	ByTravelAgent    []CostBucket           `json:"byTravelAgent"`
	ByCostType       []CostBucket           `json:"byCostType"`
	ByPaid           []CostBucket           `json:"byPaid"`
	PaidSharePercent *float64               `json:"paidSharePercent"`
	NotConverted     []CostNotConverted     `json:"notConverted"`
	FailedSources    []string               `json:"failedSources"`
	Truncated        bool                   `json:"truncated"`
	// RatesPending is true when some amounts in NotConverted are unconverted
	// only because exchange rates are still loading; reload shortly.
	RatesPending bool `json:"ratesPending,omitempty"`
	// ComparisonFailedSources are the sources that could not be read for the
	// comparison period. When any is listed, or the comparison period was cut
	// at the line cap, no change against last year is given, because last
	// year's figures are incomplete.
	ComparisonFailedSources []string   `json:"comparisonFailedSources,omitempty"`
	ComparisonTruncated     bool       `json:"comparisonTruncated,omitempty"`
	Facets                  CostFacets `json:"facets"`
}

// CostLine is one normalised cost line with its USD amount and how it was got.
type CostLine struct {
	ID               string           `json:"id"`
	Source           string           `json:"source"`
	Reference        *string          `json:"reference"`
	JobNumber        *string          `json:"jobNumber"`
	Traveller        *Person          `json:"traveller"`
	Date             string           `json:"date"`
	OriginalAmount   float64          `json:"originalAmount"`
	OriginalCurrency string           `json:"originalCurrency"`
	USDAmount        *float64         `json:"usdAmount"`
	Rate             *float64         `json:"rate"`
	RateDate         *string          `json:"rateDate"`
	RateSource       *string          `json:"rateSource"`
	CostType         string           `json:"costType"`
	SourceType       *string          `json:"sourceType"`
	Country          *string          `json:"country"`
	TravelAgent      *CostTravelAgent `json:"travelAgent"`
	Customer         *string          `json:"customer"`
	Paid             string           `json:"paid"`
	EngagementID     *string          `json:"engagementId"`
	AllocationType   *string          `json:"allocationType"`
}

// CostLinePage is the transactions answer without grouping.
type CostLinePage struct {
	Items    []CostLine `json:"items"`
	Total    int        `json:"total"`
	TotalUSD float64    `json:"totalUsd"`
	DataAsOf string     `json:"dataAsOf"`
}

// CostGroup is one group of the transactions answer.
type CostGroup struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Lines         int      `json:"lines"`
	Trips         int      `json:"trips"`
	Travellers    int      `json:"travellers"`
	Total         float64  `json:"total"`
	Previous      *float64 `json:"previous"`
	ChangePercent *float64 `json:"changePercent"`
	Thin          bool     `json:"thin"`
}

// CostGroupPage is the transactions answer with groupBy.
type CostGroupPage struct {
	Groups   []CostGroup `json:"groups"`
	Total    int         `json:"total"`
	DataAsOf string      `json:"dataAsOf"`
}

// JobCostSource is one source of a trip's total cost so far.
type JobCostSource struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	USD   float64 `json:"usd"`
	Lines int     `json:"lines"`
}

// JobCostSummary is GET /travel-requests/{jobNumber}/cost-summary: what the
// trip has cost so far, from the same lines and rates as Travel Costs.
// BySource is left out for a travel agent.
type JobCostSummary struct {
	TotalUSD float64         `json:"totalUsd"`
	BySource []JobCostSource `json:"bySource,omitempty"`
	// NotConverted and FailedSources name only the sources the caller may
	// view. HasUnconvertedHidden says an amount in a source they may not view
	// could not be converted, with no detail.
	NotConverted         []CostNotConverted `json:"notConverted"`
	HasUnconvertedHidden bool               `json:"hasUnconvertedHidden"`
	// Incomplete is true when any source could not be read or the line cap was
	// reached, so the total may be too low.
	Incomplete    bool     `json:"incomplete"`
	FailedSources []string `json:"failedSources"`
	// RatesPending is true when exchange rates are still loading, so some
	// amounts are unconverted for now.
	RatesPending bool   `json:"ratesPending,omitempty"`
	DataAsOf     string `json:"dataAsOf"`
}
