package extract

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/Ann997943/jobscout/internal/model"
)

// Fake Messages API: checks the request shape and returns a canned extraction.
func TestExtractRequestAndParse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if r.Header.Get("anthropic-beta") != "server-side-fallback-2026-07-01" {
			t.Errorf("beta header = %q", r.Header.Get("anthropic-beta"))
		}
		out, _ := json.Marshal(map[string]any{
			"grad_earliest": "2027-12", "grad_latest": "2028-12", "degree_levels": []string{"bachelors", "masters"},
			"sponsorship": "no_sponsorship", "us_citizenship_required": "not_stated", "security_clearance_required": "no",
			"work_mode": "onsite", "season": "Summer 2027",
			"evidence": []map[string]string{{"field": "grad_earliest", "quote": "graduating between December 2027 and December 2028"}},
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"content":     []map[string]any{{"type": "text", "text": string(out)}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 10, "output_tokens": 10},
		})
	}))
	defer srv.Close()

	ex := New("claude-opus-5-5", option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	e, err := ex.Extract(context.Background(), model.Job{ATS: "greenhouse", ExternalID: "1", Company: "Tesla", Title: "Software Engineering Intern",
		Description: "Must be graduating between December 2027 and December 2028. Requires 40 hours on-site."})
	if err != nil {
		t.Fatal(err)
	}
	if e.GradEarliest != "2027-12" || e.GradLatest != "2028-12" || e.Sponsorship != SponsorshipNone || e.WorkMode != "onsite" {
		t.Errorf("parsed %+v", e)
	}

	oc, _ := got["output_config"].(map[string]any)
	fm, _ := oc["format"].(map[string]any)
	if oc["effort"] != "low" || fm["type"] != "json_schema" || fm["schema"] == nil {
		t.Errorf("output_config = %v", got["output_config"])
	}
	if got["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", got["fallbacks"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking must be omitted on Opus 5.5")
	}
}
