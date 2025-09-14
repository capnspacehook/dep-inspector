package main

import (
	"maps"
	"slices"
	"strings"

	"github.com/samber/lo"
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

	t.Caps = lo.CountValuesBy(caps, func(c *capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(c.Capability, "CAPABILITY_"), "_", " ")
		//lint:ignore SA1019 the capability name will not have Unicode
		// punctuation that causes issues for strings.ToLower so using
		// it is fine
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
	currentTotalFindings := maps.Clone(sameFindings)
	deltaTotalFindings := make(map[string]int, len(sameFindings)+len(newFindings))

	allNames := append(slices.Collect(maps.Keys(rmFindings)), slices.Collect(maps.Keys(sameFindings))...)
	allNames = append(allNames, slices.Collect(maps.Keys(newFindings))...)
	slices.Sort(allNames)
	allNames = slices.Compact(allNames)

	for _, name := range allNames {
		total := sameFindings[name] + newFindings[name]

		currentTotalFindings[name] = total
		deltaTotalFindings[name] = newFindings[name] - rmFindings[name]
		grandTotal += total
	}

	return grandTotal, currentTotalFindings, deltaTotalFindings
}
