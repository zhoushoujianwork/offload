// Package text holds the small string helpers shared by the CLI and the worker.
package text

import "strings"

// OneLine collapses all whitespace runs, including newlines, into single spaces.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Truncate cuts s to n runes and marks the cut.
func Truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
