package source

import "testing"

func TestParseATSURL(t *testing.T) {
	cases := []struct{ url, ats, board, id string }{
		{"https://job-boards.greenhouse.io/figma/jobs/5612345004", "greenhouse", "figma", "5612345004"},
		{"https://boards.greenhouse.io/embed/job_app?for=janestreet&token=7012345", "greenhouse", "janestreet", "7012345"},
		{"https://stripe.com/jobs/search?gh_jid=8172508", "greenhouse", "", "8172508"},
		{"https://jobs.lever.co/palantir/6ed76ce8-4156-4b60-b120-403538bd66cd/apply", "lever", "palantir", "6ed76ce8-4156-4b60-b120-403538bd66cd"},
		{"https://jobs.ashbyhq.com/ramp/34413f8d-26bf-4bbc-8ade-eb309a0e2245?utm_source=Simplify", "ashby", "ramp", "34413f8d-26bf-4bbc-8ade-eb309a0e2245"},
		{"https://jobs.ashbyhq.com/skyband%20systems/34413f8d-26bf-4bbc-8ade-eb309a0e2245", "ashby", "skyband systems", "34413f8d-26bf-4bbc-8ade-eb309a0e2245"},
	}
	for _, c := range cases {
		p, ok := ParseATSURL(c.url)
		if !ok || p.ATS != c.ats || p.Board != c.board || p.ExternalID != c.id {
			t.Errorf("%s => %+v ok=%v", c.url, p, ok)
		}
	}
	if _, ok := ParseATSURL("https://generalmotors.wd5.myworkdayjobs.com/job/123"); ok {
		t.Error("workday should not parse")
	}
}
