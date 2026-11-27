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
	"time"

	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/clients/engagemententity"
	"github.com/wso2-open-operations/finance-tools/apps/engagement-app/backend/internal/domain"
)

// JobCostSummary is the cost so far of one travel request: every line of the
// job, in every source, over all its dates. It is read with the same line
// builder and rate converter as Travel Costs, so the total is the sum of the
// same per-line cent amounts that screen shows when filtered to the job.
//
// Who may ask is who may open the request (travelScope: full, the assigned
// agency, or the traveller), and a missing request is a 404 as on the detail
// endpoint. A travel agent gets the total but no per-source split.
func (s *dashboardService) JobCostSummary(ctx context.Context, p domain.Perms, jobNumber string) (*domain.JobCostSummary, error) {
	if !travelJobNumber.MatchString(jobNumber) {
		return nil, travelBadRequest("Invalid job number format")
	}
	trip, err := s.entity.GetTravelRequest(ctx, jobNumber)
	if err != nil {
		return nil, travelNotFound(err, "Travel request "+jobNumber+" was not found.")
	}
	scope := travelScope(p, trip.TravellerEmail, trip.TravelAgentID)
	if scope == "" {
		return nil, travelForbidden(travelNoAccess)
	}

	visible := jobCostVisibleSources(p, scope)
	out := &domain.JobCostSummary{NotConverted: []domain.CostNotConverted{}, FailedSources: []string{}}
	// One read of the job with no date range: its lines may be dated long
	// before the trip (an early invoice or card booking), and the entity reads
	// every date when only job numbers are given.
	// Job reads share the pod-wide bound on cost-line reads with the report loads.
	if err := s.costCache.acquire(ctx); err != nil {
		return nil, err
	}
	ectx, cancel := context.WithTimeout(ctx, costEntityBudget)
	res, err := s.entity.DashboardCostLines(ectx, engagemententity.DashboardCostLinesFilter{JobNumbers: []string{jobNumber}})
	cancel()
	s.costCache.release()
	if err != nil {
		return nil, err
	}
	set := s.buildCostSet(ctx, res)
	lines, asOf := set.lines, set.asOf
	out.Incomplete = set.truncated || len(set.failed) > 0
	out.RatesPending = set.ratesPending
	for _, f := range set.failed {
		if visible[f] && !contains(out.FailedSources, f) {
			out.FailedSources = append(out.FailedSources, f)
		}
	}
	lines = costApply(costTrim(lines, costSourceOrder), CostFilter{JobNumbers: []string{jobNumber}})

	out.TotalUSD = dashboardRound(costStatsOf(lines).total)
	out.DataAsOf = asOf.Format(time.RFC3339)
	// Only the total is for everyone who may open the request. Below it, a
	// source's lines, amounts and name follow the caller's right to view that
	// section of the trip.
	var seen, hidden []costLine
	for _, l := range lines {
		if visible[l.e.Source] {
			seen = append(seen, l)
		} else {
			hidden = append(hidden, l)
		}
	}
	out.NotConverted = costNotConverted(seen)
	out.HasUnconvertedHidden = len(costNotConverted(hidden)) > 0
	if scope != domain.TravelScopeAgency && !p.IsAgent() {
		sums, _, _ := costGroupSums(lines, sourceKey)
		counts := map[string]int{}
		for _, l := range lines {
			counts[l.e.Source]++
		}
		out.BySource = []domain.JobCostSource{}
		for _, src := range costSourceOrder {
			if !visible[src] {
				continue
			}
			out.BySource = append(out.BySource, domain.JobCostSource{
				Key: src, Label: costSourceLabel[src], USD: dashboardRound(sums[src]), Lines: counts[src],
			})
		}
	}
	return out, nil
}

// jobCostVisibleSources are the sources whose section of this trip the caller
// may open, by the trip page's own tab rules (travelTabs): Per Diems, Travel
// Invoices, and the Expenses tab that lists card spend and expense claims.
func jobCostVisibleSources(p domain.Perms, scope string) map[string]bool {
	tabs := travelTabs(p, scope)
	return map[string]bool{
		domain.CostSourceInvoice: tabs.Invoices,
		domain.CostSourcePerDiem: tabs.PerDiems,
		domain.CostSourceCard:    tabs.Expenses,
		domain.CostSourceClaim:   tabs.Expenses,
	}
}
