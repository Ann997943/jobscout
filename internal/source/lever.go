package source

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/textutil"
)

// Lever Postings API: https://github.com/lever/postings-api
// The list endpoint already includes full descriptions.
type lever struct{ c *Client }

type leverPosting struct {
	ID               string `json:"id"`
	Text             string `json:"text"`
	HostedURL        string `json:"hostedUrl"`
	CreatedAt        int64  `json:"createdAt"` // unix ms
	DescriptionPlain string `json:"descriptionPlain"`
	AdditionalPlain  string `json:"additionalPlain"`
	Lists            []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	} `json:"lists"`
	Categories struct {
		Location     string   `json:"location"`
		AllLocations []string `json:"allLocations"`
		Commitment   string   `json:"commitment"`
	} `json:"categories"`
	WorkplaceType string `json:"workplaceType"`
}

func (l lever) List(ctx context.Context, b model.Board) ([]model.Job, error) {
	var posts []leverPosting
	u := "https://api.lever.co/v0/postings/" + url.PathEscape(b.Token) + "?mode=json"
	if err := l.c.getJSON(ctx, u, &posts); err != nil {
		return nil, err
	}
	out := make([]model.Job, 0, len(posts))
	for _, p := range posts {
		var sb strings.Builder
		sb.WriteString(p.DescriptionPlain)
		for _, li := range p.Lists {
			sb.WriteString("\n\n" + li.Text + "\n" + textutil.HTMLToText(li.Content))
		}
		if p.AdditionalPlain != "" {
			sb.WriteString("\n\n" + p.AdditionalPlain)
		}
		if p.WorkplaceType != "" && p.WorkplaceType != "unspecified" {
			sb.WriteString("\n\nWorkplace type: " + p.WorkplaceType)
		}
		desc := textutil.CleanText(sb.String())
		locs := p.Categories.AllLocations
		if len(locs) == 0 && p.Categories.Location != "" {
			locs = []string{p.Categories.Location}
		}
		var posted time.Time
		if p.CreatedAt > 0 {
			posted = time.UnixMilli(p.CreatedAt)
		}
		out = append(out, model.Job{
			ATS:         model.ATSLever,
			Board:       b.Token,
			ExternalID:  p.ID,
			Company:     b.Name,
			Title:       p.Text,
			Locations:   locs,
			URL:         p.HostedURL,
			Description: desc,
			PostedAt:    posted,
			Hints:       []string{p.Categories.Commitment},
			Version:     model.HashContent(p.Text, strings.Join(locs, ";"), desc),
		})
	}
	return out, nil
}

func (l lever) Detail(_ context.Context, _ model.Board, j model.Job) (model.Job, error) {
	return j, nil
}
