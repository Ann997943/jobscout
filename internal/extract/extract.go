// Package extract turns free-text job descriptions into structured
// eligibility fields with Claude, using structured outputs so the response is
// guaranteed to match the JSON schema below.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/Ann997943/jobscout/internal/model"
)

// Values used by the categorical fields.
const (
	NotStated = "not_stated"

	SponsorshipNone      = "no_sponsorship"
	SponsorshipAvailable = "sponsorship_available"

	Yes = "yes"
	No  = "no"
)

// Eligibility is what we extract from a JD. Empty string / empty slice means
// "the posting does not say" — never a guess.
type Eligibility struct {
	GradEarliest          string     `json:"grad_earliest"`               // "YYYY-MM" or ""
	GradLatest            string     `json:"grad_latest"`                 // "YYYY-MM" or ""
	DegreeLevels          []string   `json:"degree_levels"`               // subset of bachelors|masters|phd
	Sponsorship           string     `json:"sponsorship"`                 // no_sponsorship|sponsorship_available|not_stated
	USCitizenshipRequired string     `json:"us_citizenship_required"`     // yes|no|not_stated
	ClearanceRequired     string     `json:"security_clearance_required"` // yes|no|not_stated
	WorkMode              string     `json:"work_mode"`                   // onsite|hybrid|remote|not_stated
	Season                string     `json:"season"`                      // e.g. "Summer 2027" or ""
	Evidence              []Evidence `json:"evidence"`
}

type Evidence struct {
	Field string `json:"field"`
	Quote string `json:"quote"`
}

var triState = []any{Yes, No, NotStated}

// Schema is sent as output_config.format. Structured outputs require
// additionalProperties:false and every property listed in required.
var Schema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required": []string{"grad_earliest", "grad_latest", "degree_levels", "sponsorship",
		"us_citizenship_required", "security_clearance_required", "work_mode", "season", "evidence"},
	"properties": map[string]any{
		"grad_earliest": map[string]any{"type": "string", "description": "Earliest accepted graduation month as YYYY-MM, or empty string if not stated."},
		"grad_latest":   map[string]any{"type": "string", "description": "Latest accepted graduation month as YYYY-MM, or empty string if not stated."},
		"degree_levels": map[string]any{
			"type":        "array",
			"description": "Degree programs the candidate may be enrolled in. Empty if not stated.",
			"items":       map[string]any{"type": "string", "enum": []any{"bachelors", "masters", "phd"}},
		},
		"sponsorship":                 map[string]any{"type": "string", "enum": []any{SponsorshipNone, SponsorshipAvailable, NotStated}},
		"us_citizenship_required":     map[string]any{"type": "string", "enum": triState},
		"security_clearance_required": map[string]any{"type": "string", "enum": triState},
		"work_mode":                   map[string]any{"type": "string", "enum": []any{"onsite", "hybrid", "remote", NotStated}},
		"season":                      map[string]any{"type": "string", "description": "Internship term such as 'Summer 2027', or empty string if not stated."},
		"evidence": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"field", "quote"},
				"properties": map[string]any{
					"field": map[string]any{"type": "string"},
					"quote": map[string]any{"type": "string", "description": "Verbatim sentence from the posting."},
				},
			},
		},
	},
}

const systemPrompt = `You read internship job postings and extract eligibility requirements into JSON.

Rules:
- Only report what the posting explicitly states. If something is not stated, use "" / [] / "not_stated". Never infer from company reputation or industry.
- Graduation window: convert to YYYY-MM. "Graduating between December 2027 and December 2028" -> grad_earliest "2027-12", grad_latest "2028-12". "Graduating in 2028" -> "2028-01" to "2028-12". "Graduating in Spring 2028" -> "2028-01" to "2028-06". "Expected graduation of May 2028 or later" -> grad_earliest "2028-05", grad_latest "". "Returning to school after the internship" alone does not give dates -> leave both empty.
- degree_levels: list only programs the posting accepts ("BS/MS students" -> ["bachelors","masters"]). "Pursuing a degree in CS" with no level -> [].
- sponsorship: "no_sponsorship" if it says the company will not sponsor visas, or candidates must be authorized to work without sponsorship now or in the future. "sponsorship_available" only if it explicitly offers sponsorship or explicitly welcomes students on F-1/CPT/OPT. Otherwise "not_stated". A generic equal-opportunity statement is not about sponsorship.
- us_citizenship_required: "yes" only for explicit US citizenship (or US person / ITAR / export-control restrictions limited to US persons).
- security_clearance_required: "yes" if a clearance is required or candidates must be eligible to obtain one.
- work_mode: from explicit statements like "on-site 5 days a week", "hybrid", "remote". A location alone is not enough.
- evidence: one short verbatim quote per non-empty / non-"not_stated" field.`

// Extractor calls Claude. Model defaults to claude-opus-5-5; any current model
// that supports structured outputs works (e.g. claude-haiku-5-5 for lower cost).
type Extractor struct {
	Client anthropic.Client
	Model  string
}

func New(modelID string, opts ...option.RequestOption) *Extractor {
	if modelID == "" {
		modelID = "claude-opus-5-5"
	}
	return &Extractor{Client: anthropic.NewClient(opts...), Model: modelID}
}

// ErrRefused is returned when the model (and its fallback) declined.
var ErrRefused = errors.New("model refused")

func (e *Extractor) Extract(ctx context.Context, j model.Job) (Eligibility, error) {
	var out Eligibility
	if strings.TrimSpace(j.Description) == "" {
		return out, fmt.Errorf("job %s has no description", j.Key())
	}
	user := fmt.Sprintf("Company: %s\nTitle: %s\nLocations: %s\n\n<posting>\n%s\n</posting>",
		j.Company, j.Title, j.LocationString(), j.Description)

	params := anthropic.BetaMessageNewParams{
		Model:     e.Model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: systemPrompt}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			// Simple extraction: low effort keeps latency and cost down.
			Effort: anthropic.BetaOutputConfigEffortLow,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: Schema},
		},
	}
	// Server-side refusal fallback: if the model declines (rare for JDs), the
	// API re-runs the request on a fallback model inside the same call.
	if supportsFallbacks(e.Model) {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}

	resp, err := e.Client.Beta.Messages.New(ctx, params)
	if err != nil {
		return out, err
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return out, ErrRefused
	case anthropic.BetaStopReasonMaxTokens:
		return out, fmt.Errorf("hit max_tokens extracting %s", j.Key())
	}
	var text strings.Builder
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if err := json.Unmarshal([]byte(text.String()), &out); err != nil {
		return out, fmt.Errorf("decode extraction for %s: %w", j.Key(), err)
	}
	out.normalize()
	return out, nil
}

func supportsFallbacks(m string) bool {
	switch m {
	case "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5", "claude-fable-5-2", "claude-fable-5-1":
		return true
	}
	return false
}

func (e *Eligibility) normalize() {
	for _, p := range []*string{&e.Sponsorship, &e.USCitizenshipRequired, &e.ClearanceRequired, &e.WorkMode} {
		if *p == "" {
			*p = NotStated
		}
	}
	if e.DegreeLevels == nil {
		e.DegreeLevels = []string{}
	}
}
