package main

import (
	"fmt"
	"strings"
)

func printDepComparison(results *inspectResults) {
	// print package issues
	if len(results.removedCaps) > 0 {
		fmt.Println("removed capabilities:")
		printCaps(results.removedCaps)
	}
	if len(results.staleCaps) > 0 {
		fmt.Println("stale capabilities:")
		printCaps(results.staleCaps)
	}
	if len(results.addedCaps) > 0 {
		fmt.Println("added capabilities:")
		printCaps(results.addedCaps)
	}
	fmt.Printf("total:\nremoved capabilities: %d\nstale capabilities:   %d\nadded capabilities:   %d\n",
		len(results.removedCaps),
		len(results.staleCaps),
		len(results.addedCaps),
	)

	// print linter issues
	if len(results.fixedIssues) > 0 {
		fmt.Println("fixed issues:")
		printLinterIssues(results.fixedIssues)
	}
	if len(results.staleIssues) > 0 {
		fmt.Println("stale issues:")
		printLinterIssues(results.staleIssues)
	}
	if len(results.newIssues) > 0 {
		fmt.Println("new issues:")
		printLinterIssues(results.newIssues)
	}
	fmt.Printf("total:\nfixed issues: %d\nstale issues: %d\nnew issues:   %d\n\n",
		len(results.fixedIssues),
		len(results.staleIssues),
		len(results.newIssues),
	)
}

func printCaps(caps []capability) {
	for _, cap := range caps {
		fmt.Printf("%s: %s\n", cap.Capability, cap.CapabilityType)
		for i, call := range cap.Path {
			if i == 0 {
				fmt.Println(call.Name)
				continue
			}

			if call.Site.Filename != "" {
				fmt.Printf("  %s %s:%s:%s\n",
					call.Name,
					call.Site.Filename,
					call.Site.Line,
					call.Site.Column,
				)
				continue
			}
			fmt.Printf("  %s\n", call.Name)
		}

		fmt.Print("\n\n")
	}
}

func printLinterIssues(issues []lintIssue) {
	for _, issue := range issues {
		srcLines := strings.Join(issue.SourceLines, "\n")

		fmt.Printf("(%s) %s: %s:%d:%d:\n%s\n\n", issue.FromLinter, issue.Text, issue.Pos.Filename, issue.Pos.Line, issue.Pos.Column, srcLines)
	}
}
