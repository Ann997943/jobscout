package dedup

import "testing"

func TestTitleSimilarity(t *testing.T) {
	same := [][2]string{
		{"Software Engineer Intern", "Software Engineering Internship"},
		{"Software Engineer Intern (Summer 2027)", "Software Engineer, Intern - Summer 2027"},
		{"SWE Intern", "Software Engineer Intern"},
		{"Machine Learning Engineer Intern", "ML Engineer Intern 2027"},
	}
	for _, p := range same {
		if s := TitleSimilarity(p[0], p[1]); s < Threshold {
			t.Errorf("%q vs %q = %.2f, want >= %.2f", p[0], p[1], s, Threshold)
		}
	}
	diff := [][2]string{
		{"Software Engineer Intern", "Product Manager Intern"},
		{"Hardware Engineer Intern", "Software Engineer Intern"},
		{"Data Science Intern", "Data Engineer Intern"},
	}
	for _, p := range diff {
		if s := TitleSimilarity(p[0], p[1]); s >= Threshold {
			t.Errorf("%q vs %q = %.2f, want < %.2f", p[0], p[1], s, Threshold)
		}
	}
}

func TestNormalizeCompany(t *testing.T) {
	if NormalizeCompany("Stripe, Inc.") != NormalizeCompany("stripe") {
		t.Error("corporate suffix not stripped")
	}
	if NormalizeCompany("Jane Street") != "janestreet" {
		t.Error(NormalizeCompany("Jane Street"))
	}
}
