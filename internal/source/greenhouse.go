package source

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/textutil"
)

// Greenhouse Job Board API: https://developers.greenhouse.io/job-board.html
//
// The list endpoint without ?content=true is small and carries updated_at, so
// we poll that and only fetch the full posting for new/changed jobs.
type greenhouse struct{ c *Client }

type ghJob struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	AbsoluteURL    string `json:"absolute_url"`
	UpdatedAt      string `json:"updated_at"`
	FirstPublished string `json:"first_published"`
	CompanyName    string `json:"company_name"`
	Content        string `json:"content"`
	Location       struct {
		Name string `json:"name"`
	} `json:"location"`
	Offices []struct {
		Name string `json:"name"`
	} `json:"offices"`
	Metadata []struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	} `json:"metadata"`
}

// hints pulls employment-type-like custom fields, e.g. Jane Street titles
// interns "Software Engineer" but sets Employment Type = "Summer Internship".
func (j ghJob) hints() []string {
	var out []string
	for _, m := range j.Metadata {
		n := strings.ToLower(m.Name)
		if !strings.Contains(n, "type") && !strings.Contains(n, "employment") && !strings.Contains(n, "profile") && !strings.Contains(n, "level") {
			continue
		}
		switch v := m.Value.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func (g greenhouse) base(b model.Board) string {
	return "https://boards-api.greenhouse.io/v1/boards/" + url.PathEscape(b.Token)
}

func (g greenhouse) List(ctx context.Context, b model.Board) ([]model.Job, error) {
	var resp struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := g.c.getJSON(ctx, g.base(b)+"/jobs", &resp); err != nil {
		return nil, err
	}
	out := make([]model.Job, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		out = append(out, g.convert(b, j))
	}
	return out, nil
}

func (g greenhouse) Detail(ctx context.Context, b model.Board, j model.Job) (model.Job, error) {
	var gj ghJob
	if err := g.c.getJSON(ctx, fmt.Sprintf("%s/jobs/%s", g.base(b), j.ExternalID), &gj); err != nil {
		return j, err
	}
	full := g.convert(b, gj)
	full.Description = textutil.HTMLToText(gj.Content)
	return full, nil
}

func (g greenhouse) convert(b model.Board, j ghJob) model.Job {
	company := j.CompanyName
	if company == "" {
		company = b.Name
	}
	locs := []string{}
	if j.Location.Name != "" {
		locs = append(locs, j.Location.Name)
	}
	posted := parseTime(j.FirstPublished)
	return model.Job{
		ATS:        model.ATSGreenhouse,
		Board:      b.Token,
		ExternalID: strconv.FormatInt(j.ID, 10),
		Company:    company,
		Title:      j.Title,
		Locations:  locs,
		URL:        j.AbsoluteURL,
		PostedAt:   posted,
		Version:    j.UpdatedAt,
		Hints:      j.hints(),
	}
}
