package source

import (
	"context"
	"net/url"
	"strings"

	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/textutil"
)

// Ashby public job posting API:
// https://developers.ashbyhq.com/docs/public-job-posting-api
type ashby struct{ c *Client }

type ashbyJob struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Location           string `json:"location"`
	SecondaryLocations []struct {
		Location string `json:"location"`
	} `json:"secondaryLocations"`
	JobURL           string `json:"jobUrl"`
	PublishedAt      string `json:"publishedAt"`
	IsListed         bool   `json:"isListed"`
	WorkplaceType    string `json:"workplaceType"`
	EmploymentType   string `json:"employmentType"`
	DescriptionPlain string `json:"descriptionPlain"`
	Compensation     *struct {
		CompensationTierSummary string `json:"compensationTierSummary"`
	} `json:"compensation"`
}

func (a ashby) List(ctx context.Context, b model.Board) ([]model.Job, error) {
	var resp struct {
		Jobs []ashbyJob `json:"jobs"`
	}
	u := "https://api.ashbyhq.com/posting-api/job-board/" + url.PathEscape(b.Token) + "?includeCompensation=true"
	if err := a.c.getJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	out := make([]model.Job, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		if !j.IsListed {
			continue
		}
		locs := []string{}
		if j.Location != "" {
			locs = append(locs, j.Location)
		}
		for _, s := range j.SecondaryLocations {
			if s.Location != "" {
				locs = append(locs, s.Location)
			}
		}
		desc := textutil.CleanText(j.DescriptionPlain)
		var extra []string
		if j.WorkplaceType != "" {
			extra = append(extra, "Workplace type: "+j.WorkplaceType)
		}
		if j.EmploymentType != "" {
			extra = append(extra, "Employment type: "+j.EmploymentType)
		}
		if j.Compensation != nil && j.Compensation.CompensationTierSummary != "" {
			extra = append(extra, "Compensation: "+j.Compensation.CompensationTierSummary)
		}
		if len(extra) > 0 {
			desc += "\n\n" + strings.Join(extra, "\n")
		}
		out = append(out, model.Job{
			ATS:         model.ATSAshby,
			Board:       b.Token,
			ExternalID:  j.ID,
			Company:     b.Name,
			Title:       strings.TrimSpace(j.Title),
			Locations:   locs,
			URL:         j.JobURL,
			Description: desc,
			PostedAt:    parseTime(j.PublishedAt),
			Hints:       []string{j.EmploymentType},
			Version:     model.HashContent(j.Title, strings.Join(locs, ";"), desc),
		})
	}
	return out, nil
}

func (a ashby) Detail(_ context.Context, _ model.Board, j model.Job) (model.Job, error) {
	return j, nil
}
