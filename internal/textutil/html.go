// Package textutil converts ATS HTML job descriptions into plain text that is
// cheap to store and to send to an LLM.
package textutil

import (
	"html"
	"regexp"
	"strings"
)

var (
	blockTag   = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/h[1-6]|/tr|/ul|/ol)\s*/?>`)
	liTag      = regexp.MustCompile(`(?i)<\s*li(\s[^>]*)?>`)
	anyTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRun   = regexp.MustCompile(`[ \t\x{00a0}]+`)
	newlineRun = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

// CleanText normalizes whitespace in text that is already plain.
func CleanText(s string) string {
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(newlineRun.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// HTMLToText strips tags while keeping paragraph and bullet structure.
// Greenhouse double-escapes its HTML (&lt;p&gt;), so unescape first.
func HTMLToText(s string) string {
	if s == "" {
		return ""
	}
	s = html.UnescapeString(s)
	s = liTag.ReplaceAllString(s, "\n- ")
	s = blockTag.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = newlineRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
