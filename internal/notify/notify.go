// Package notify sends alerts for newly found postings.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
)

// Alert is one job worth telling the user about.
type Alert struct {
	Job        model.Job
	OtherLocs  []string // locations of duplicate postings merged into this alert
	Extraction *extract.Eligibility
	Verdict    *filter.Verdict
	FoundAt    time.Time
}

type Notifier interface {
	Send(ctx context.Context, a Alert) error
}

// Format renders a compact, chat-friendly message.
func Format(a Alert) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s** — %s\n", a.Job.Company, a.Job.Title)
	locs := append(append([]string{}, a.Job.Locations...), a.OtherLocs...)
	if len(locs) > 0 {
		fmt.Fprintf(&sb, "📍 %s\n", strings.Join(dedupe(locs), " · "))
	}
	if e := a.Extraction; e != nil {
		var parts []string
		if e.GradEarliest != "" || e.GradLatest != "" {
			parts = append(parts, fmt.Sprintf("grad %s–%s", or(e.GradEarliest, "?"), or(e.GradLatest, "?")))
		}
		if e.Season != "" {
			parts = append(parts, e.Season)
		}
		if e.Sponsorship != extract.NotStated {
			parts = append(parts, strings.ReplaceAll(e.Sponsorship, "_", " "))
		}
		if e.WorkMode != extract.NotStated {
			parts = append(parts, e.WorkMode)
		}
		if len(parts) > 0 {
			fmt.Fprintf(&sb, "🧾 %s\n", strings.Join(parts, " · "))
		}
	}
	if v := a.Verdict; v != nil && len(v.Unknown) > 0 {
		fmt.Fprintf(&sb, "❔ not stated: %s\n", strings.Join(v.Unknown, ", "))
	}
	if !a.Job.PostedAt.IsZero() {
		lag := a.FoundAt.Sub(a.Job.PostedAt).Round(time.Minute)
		if lag >= 0 {
			fmt.Fprintf(&sb, "⏱ found %s after posting\n", lag)
		}
	}
	sb.WriteString(a.Job.URL)
	return sb.String()
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Stdout prints alerts to the terminal.
type Stdout struct{}

func (Stdout) Send(_ context.Context, a Alert) error {
	fmt.Fprintf(os.Stdout, "\n🆕 %s\n", strings.ReplaceAll(Format(a), "**", ""))
	return nil
}

// Webhook posts to Discord or Slack incoming webhooks.
type Webhook struct {
	URL  string
	Kind string // "discord" | "slack"
	HTTP *http.Client
}

func (w Webhook) Send(ctx context.Context, a Alert) error {
	msg := Format(a)
	var payload any
	switch w.Kind {
	case "discord":
		if len(msg) > 1900 {
			msg = msg[:1900] + "…"
		}
		payload = map[string]any{"content": msg, "flags": 4} // 4 = suppress link embeds
	case "slack":
		payload = map[string]any{"text": strings.ReplaceAll(msg, "**", "*")}
	default:
		return fmt.Errorf("unknown webhook kind %q", w.Kind)
	}
	body, _ := json.Marshal(payload)
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s webhook: HTTP %d", w.Kind, resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("%s webhook: rate limited", w.Kind)
}

// Multi fans out to several notifiers.
type Multi []Notifier

func (m Multi) Send(ctx context.Context, a Alert) error {
	var errs []string
	for _, n := range m {
		if err := n.Send(ctx, a); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify: %s", strings.Join(errs, "; "))
	}
	return nil
}

// FromEnv builds notifiers from DISCORD_WEBHOOK_URL / SLACK_WEBHOOK_URL and
// always includes stdout.
func FromEnv() Notifier {
	m := Multi{Stdout{}}
	if u := os.Getenv("DISCORD_WEBHOOK_URL"); u != "" {
		m = append(m, Webhook{URL: u, Kind: "discord"})
	}
	if u := os.Getenv("SLACK_WEBHOOK_URL"); u != "" {
		m = append(m, Webhook{URL: u, Kind: "slack"})
	}
	return m
}
