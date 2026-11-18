package filter

import (
	"testing"

	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/model"
)

func TestIsInternship(t *testing.T) {
	yes := []string{"AI Research Fellowship, (Summer 2026)", "Contract Student Worker – Firmware", "Software Engineer Intern", "Internship - Data", "Co-op, Firmware", "Summer Analyst 2027", "Interns: ML", "Student Researcher, PhD"}
	no := []string{"Internal Tools Engineer", "International Tax Manager", "Senior Software Engineer", "Internal Audit Lead"}
	for _, s := range yes {
		if !IsInternship(s) {
			t.Errorf("IsInternship(%q) = false", s)
		}
	}
	for _, s := range no {
		if IsInternship(s) {
			t.Errorf("IsInternship(%q) = true", s)
		}
	}
}

func TestCheck(t *testing.T) {
	p := Profile{Graduation: "2028-05", Degree: "bachelors", NeedsSponsorship: true}
	base := extract.Eligibility{Sponsorship: extract.NotStated, USCitizenshipRequired: extract.NotStated, ClearanceRequired: extract.NotStated, WorkMode: extract.NotStated}

	e := base
	e.GradEarliest, e.GradLatest = "2027-12", "2028-12" // the Tesla example
	if v := p.Check(e); !v.Eligible {
		t.Errorf("in window should be eligible: %v", v.Reasons)
	}
	e.GradLatest = "2028-03"
	if v := p.Check(e); v.Eligible {
		t.Error("graduating after window should be ineligible")
	}

	e = base
	e.Sponsorship = extract.SponsorshipNone
	if v := p.Check(e); v.Eligible {
		t.Error("no sponsorship should exclude a student who needs it")
	}

	e = base
	e.DegreeLevels = []string{"masters", "phd"}
	if v := p.Check(e); v.Eligible {
		t.Error("bachelors student excluded from MS/PhD posting")
	}

	if v := p.Check(base); !v.Eligible || len(v.Unknown) == 0 {
		t.Errorf("unknowns must not disqualify but should be reported: %+v", v)
	}
}

func TestIsInternshipJobHints(t *testing.T) {
	if !IsInternshipJob(model.Job{Title: "Software Engineer", Hints: []string{"Summer Internship"}}) {
		t.Error("Greenhouse employment-type hint ignored")
	}
	if !IsInternshipJob(model.Job{Title: "Software Engineer", Hints: []string{"Intern"}}) {
		t.Error("Ashby employmentType=Intern ignored")
	}
	if IsInternshipJob(model.Job{Title: "Software Engineer", Hints: []string{"FullTime", "Full-time"}}) {
		t.Error("full-time misclassified")
	}
}

func TestTitleMatchesWholeWords(t *testing.T) {
	p := Profile{TitleInclude: []string{"ai", "ml", "software"}, TitleExclude: []string{"hr"}}
	cases := map[string]bool{
		"AI Research Intern":          true,
		"ML/Infra Intern":             true,
		"Supply Chain Intern":         false,
		"Software Engineer Intern":    true,
		"HR Software Intern":          false,
		"Three-phase Software Intern": true, // "hr" inside "three" must not exclude
	}
	for title, want := range cases {
		if got := p.TitleMatches(title); got != want {
			t.Errorf("TitleMatches(%q) = %v, want %v", title, got, want)
		}
	}
}
