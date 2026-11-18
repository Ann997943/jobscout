// Package source fetches postings from public ATS job-board APIs.
//
// Only official, unauthenticated, read-only endpoints that the ATS vendors
// publish for embedding job boards are used. No scraping of LinkedIn/Indeed.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Ann997943/jobscout/internal/model"
)

// Fetcher lists postings on a board. List may return jobs without a
// Description when the provider's list endpoint is lightweight; Detail fills
// it in and is only called for jobs that are new or changed AND relevant.
type Fetcher interface {
	List(ctx context.Context, b model.Board) ([]model.Job, error)
	Detail(ctx context.Context, b model.Board, j model.Job) (model.Job, error)
}

// For returns the fetcher for an ATS name.
func For(ats string, c *Client) (Fetcher, error) {
	switch ats {
	case model.ATSGreenhouse:
		return greenhouse{c}, nil
	case model.ATSLever:
		return lever{c}, nil
	case model.ATSAshby:
		return ashby{c}, nil
	}
	return nil, fmt.Errorf("unsupported ATS %q", ats)
}

// ErrNotFound means the board token does not exist (or was removed).
var ErrNotFound = fmt.Errorf("board not found")

// Client is a small HTTP client with retry on 429/5xx that honours Retry-After.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	Retries   int
}

func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "jobscout/0.1 (personal job alert bot)",
		Retries:   3,
	}
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if !sleep(ctx, backoff(attempt, "")) {
				return ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
			if !sleep(ctx, backoff(attempt, resp.Header.Get("Retry-After"))) {
				return ctx.Err()
			}
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		if readErr != nil {
			return readErr
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("GET %s: decode: %w", url, err)
		}
		return nil
	}
	return lastErr
}

func backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s > 0 && s < 600 {
		return time.Duration(s) * time.Second
	}
	return time.Duration(1<<attempt) * time.Second
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
