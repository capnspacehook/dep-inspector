package main

import (
	"strings"

	"github.com/samber/lo"
	"golang.org/x/exp/maps"
)

type findingTotals struct {
	HasDeltas bool

	TotalCaps   int
	Caps        map[string]int
	CapDeltas   map[string]int
	TotalIssues int
	Issues      map[string]int
	IssueDeltas map[string]int
}

func calculateTotals(caps []*capability, issues []*lintIssue) findingTotals {
	t := findingTotals{
		TotalCaps:   len(caps),
		TotalIssues: len(issues),
	}

	t.Caps = lo.CountValuesBy(caps, func(cap *capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(cap.Capability, "CAPABILITY_"), "_", " ")
		return strings.Title(strings.ToLower(capName))
	})
	t.Issues = lo.CountValuesBy(issues, func(issue *lintIssue) string {
		if strings.HasPrefix(issue.FromLinter, "staticcheck") {
			return "staticcheck"
		}
		return issue.FromLinter
	})
	return t
}

func buildCombinedTotals(r *compareDepsResult) {
	totalCaps, capTotals, capDeltas := currentTotals(
		r.OldFindings.Totals.Caps,
		r.SameFindings.Totals.Caps,
		r.NewFindings.Totals.Caps,
	)
	totalIssues, issueTotals, issueDeltas := currentTotals(
		r.OldFindings.Totals.Issues,
		r.SameFindings.Totals.Issues,
		r.NewFindings.Totals.Issues,
	)
	r.Totals = findingTotals{
		HasDeltas:   true,
		TotalCaps:   totalCaps,
		Caps:        capTotals,
		CapDeltas:   capDeltas,
		TotalIssues: totalIssues,
		Issues:      issueTotals,
		IssueDeltas: issueDeltas,
	}
}

func currentTotals(rmFindings, sameFindings, newFindings map[string]int) (int, map[string]int, map[string]int) {
	grandTotal := 0
	currentCapTotals := maps.Clone(sameFindings)
	deltaCapTotals := make(map[string]int, len(sameFindings)+len(newFindings))

	for capName, newTotal := range newFindings {
		total, ok := currentCapTotals[capName]
		deltaCapTotals[capName] = total + newTotal - rmFindings[capName]
		if ok {
			total += newTotal
		}
		currentCapTotals[capName] = total
		grandTotal += total
	}

	return grandTotal, currentCapTotals, deltaCapTotals
}
