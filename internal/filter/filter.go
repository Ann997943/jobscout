// Package filter decides which postings are internships and whether the user
// is eligible for them, based on a profile and the LLM-extracted fields.
package filter

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/model"
)

var internRe = regexp.MustCompile(`(?i)\b(interns?|internships?|co-?ops?|summer (analyst|associate|engineer|scholar)s?|student (researcher|engineer|worker)s?|apprentice(ship)?s?|fellowships?|fellows program|scholars?|studentships?|placements?)\b`)

// IsInternship is a cheap text check (title or employment-type field).
func IsInternship(s string) bool { return internRe.MatchString(s) }

// IsInternshipJob checks the title and the ATS's structured hints.
func IsInternshipJob(j model.Job) bool {
	if IsInternship(j.Title) {
		return true
	}
	for _, h := range j.Hints {
		if IsInternship(h) {
			return true
		}
	}
	return false
}

// Profile describes the user. Loaded from profile.yaml.
type Profile struct {
	Graduation       string   `yaml:"graduation"`        // YYYY-MM
	Degree           string   `yaml:"degree"`            // bachelors|masters|phd
	NeedsSponsorship bool     `yaml:"needs_sponsorship"` // e.g. international student
	USCitizen        bool     `yaml:"us_citizen"`
	HasClearance     bool     `yaml:"has_clearance"`
	TitleInclude     []string `yaml:"title_include"` // any-of, case-insensitive; empty = all
	TitleExclude     []string `yaml:"title_exclude"`
	Locations        []string `yaml:"locations"` // any-of substring; empty = anywhere
	AllowOnsite      *bool    `yaml:"allow_onsite"`
}

func LoadProfile(path string) (Profile, error) {
	var p Profile
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// containsAny matches whole words/phrases, case-insensitively, so the
// keyword "ai" matches "AI Engineer" but not "Supply Chain".
func containsAny(hay string, needles []string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		re, ok := wordRes.Load(n)
		if !ok {
			re, _ = wordRes.LoadOrStore(n, regexp.MustCompile(`(?i)(^|[^\pL\pN])`+regexp.QuoteMeta(n)+`($|[^\pL\pN])`))
		}
		if re.(*regexp.Regexp).MatchString(hay) {
			return true
		}
	}
	return false
}

var wordRes sync.Map

// TitleMatches applies include/exclude keywords.
func (p Profile) TitleMatches(title string) bool {
	if containsAny(title, p.TitleExclude) {
		return false
	}
	return len(p.TitleInclude) == 0 || containsAny(title, p.TitleInclude)
}

// LocationMatches is lenient: postings with no location pass.
func (p Profile) LocationMatches(locs []string) bool {
	if len(p.Locations) == 0 || len(locs) == 0 {
		return true
	}
	return containsAny(strings.Join(locs, " | "), p.Locations)
}

// Verdict is the result of checking a profile against extracted requirements.
type Verdict struct {
	Eligible bool
	Reasons  []string // why not eligible
	Unknown  []string // requirements the posting doesn't state
}

// Check compares profile and extraction. Missing info never disqualifies —
// it is reported in Unknown so the user can decide.
func (p Profile) Check(e extract.Eligibility) Verdict {
	v := Verdict{Eligible: true}
	fail := func(f string, a ...any) { v.Eligible = false; v.Reasons = append(v.Reasons, fmt.Sprintf(f, a...)) }

	if p.Graduation != "" {
		if e.GradEarliest == "" && e.GradLatest == "" {
			v.Unknown = append(v.Unknown, "graduation window")
		}
		// YYYY-MM strings compare correctly lexicographically.
		if e.GradEarliest != "" && p.Graduation < e.GradEarliest {
			fail("graduates %s, posting wants %s or later", p.Graduation, e.GradEarliest)
		}
		if e.GradLatest != "" && p.Graduation > e.GradLatest {
			fail("graduates %s, posting wants %s or earlier", p.Graduation, e.GradLatest)
		}
	}
	if p.Degree != "" && len(e.DegreeLevels) > 0 {
		ok := false
		for _, d := range e.DegreeLevels {
			ok = ok || d == p.Degree
		}
		if !ok {
			fail("degree %s not in %v", p.Degree, e.DegreeLevels)
		}
	}
	if p.NeedsSponsorship {
		switch e.Sponsorship {
		case extract.SponsorshipNone:
			fail("no visa sponsorship")
		case extract.NotStated:
			v.Unknown = append(v.Unknown, "sponsorship")
		}
	}
	if !p.USCitizen && e.USCitizenshipRequired == extract.Yes {
		fail("US citizenship required")
	}
	if !p.HasClearance && e.ClearanceRequired == extract.Yes && !p.USCitizen {
		// Clearances generally require citizenship; citizens can usually obtain one.
		fail("security clearance required")
	}
	if p.AllowOnsite != nil && !*p.AllowOnsite && e.WorkMode == "onsite" {
		fail("on-site only")
	}
	return v
}
