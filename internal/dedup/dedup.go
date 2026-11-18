// Package dedup decides whether two postings are "the same job" even when
// titles are worded differently or the role is posted once per location.
package dedup

import (
	"regexp"
	"sort"
	"strings"
)

var (
	nonAlnum    = regexp.MustCompile(`[^a-z0-9+#]+`)
	yearRe      = regexp.MustCompile(`^(19|20)\d\d$`)
	corpSuffix  = regexp.MustCompile(`\b(inc|llc|ltd|corp|corporation|co|company|technologies|technology|labs|group|holdings)\b`)
	parenthetic = regexp.MustCompile(`\([^)]*\)|\[[^\]]*\]`)
)

// Canonical spellings so "Software Engineering Internship" == "Software Engineer Intern".
var synonyms = map[string]string{
	"engineering": "engineer", "engineers": "engineer", "eng": "engineer", "swe": "software engineer",
	"internship": "intern", "internships": "intern", "interns": "intern", "co-op": "intern", "coop": "intern",
	"developer": "engineer", "development": "engineer", "dev": "engineer",
	"ml": "machine learning", "ai": "artificial intelligence",
	"sw": "software", "hw": "hardware", "sde": "software engineer",
	"undergraduate": "undergrad", "graduate": "grad",
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "and": true, "for": true, "to": true, "in": true,
	"summer": true, "fall": true, "winter": true, "spring": true, "autumn": true,
	"2025": true, "2026": true, "2027": true, "2028": true, "2029": true, "&": true,
	"remote": true, "hybrid": true, "onsite": true, "us": true, "usa": true,
}

// NormalizeCompany lowercases and strips punctuation and corporate suffixes.
func NormalizeCompany(s string) string {
	s = strings.ToLower(s)
	s = corpSuffix.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(nonAlnum.ReplaceAllString(s, " ")), "")
}

// TitleTokens returns the canonical token set of a job title.
func TitleTokens(title string) []string {
	s := strings.ToLower(title)
	s = parenthetic.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "co-op", "coop")
	set := map[string]bool{}
	for _, w := range strings.Fields(nonAlnum.ReplaceAllString(s, " ")) {
		if syn, ok := synonyms[w]; ok {
			w = syn
		}
		for _, t := range strings.Fields(w) {
			if !stopwords[t] && !yearRe.MatchString(t) {
				set[t] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// TitleSimilarity is the Jaccard index of the canonical token sets.
func TitleSimilarity(a, b string) float64 {
	ta, tb := TitleTokens(a), TitleTokens(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1
	}
	set := map[string]int{}
	for _, t := range ta {
		set[t] |= 1
	}
	for _, t := range tb {
		set[t] |= 2
	}
	inter := 0
	for _, v := range set {
		if v == 3 {
			inter++
		}
	}
	return float64(inter) / float64(len(set))
}

// Threshold above which two titles at the same company are treated as one job.
const Threshold = 0.75

func SameJob(companyA, titleA, companyB, titleB string) bool {
	return NormalizeCompany(companyA) == NormalizeCompany(companyB) &&
		TitleSimilarity(titleA, titleB) >= Threshold
}
