// Package web serves a small dashboard and JSON API over the job store.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/pipeline"
	"github.com/Ann997943/jobscout/internal/store"
)

//go:embed static
var static embed.FS

type Server struct {
	Store    *store.Store
	Profile  filter.Profile
	Pipeline *pipeline.Pipeline // nil = read-only dashboard
	Boards   []model.Board

	mu      sync.Mutex
	polling bool
	last    *pollInfo
}

type pollInfo struct {
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Failed  int       `json:"failed"`
	TookMS  int64     `json:"took_ms"`
}

// PollLoop polls immediately, then every interval, until ctx is done.
func (s *Server) PollLoop(ctx context.Context, interval time.Duration) {
	for {
		s.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// PollOnce runs one poll unless one is already in progress.
func (s *Server) PollOnce(ctx context.Context) bool {
	if s.Pipeline == nil {
		return false
	}
	s.mu.Lock()
	if s.polling {
		s.mu.Unlock()
		return false
	}
	s.polling = true
	s.mu.Unlock()

	st := s.Pipeline.PollAll(ctx, s.Boards)
	slog.Info("poll done", "summary", st.String())

	s.mu.Lock()
	s.polling = false
	s.last = &pollInfo{At: time.Now(), Summary: st.String(), Failed: st.Failed, TookMS: st.Took.Milliseconds()}
	s.mu.Unlock()
	return true
}

// Handler serves the page and a live jobs.json — the same two files that
// `jobscout export` writes for static hosting (GitHub Pages).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /jobs.json", func(w http.ResponseWriter, r *http.Request) {
		snap, err := BuildSnapshot(r.Context(), s.Store, s.Profile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, snap)
	})
	return mux
}

// IndexHTML returns the embedded page, for static export.
func IndexHTML() ([]byte, error) { return static.ReadFile("static/index.html") }

type jobJSON struct {
	Key        string               `json:"key"`
	ATS        string               `json:"ats"`
	Company    string               `json:"company"`
	Title      string               `json:"title"`
	Locations  []string             `json:"locations"`
	URL        string               `json:"url"`
	PostedAt   *time.Time           `json:"posted_at,omitempty"`
	FirstSeen  time.Time            `json:"first_seen"`
	New        bool                 `json:"new"` // appeared after we started tracking its board
	TitleMatch bool                 `json:"title_match"`
	Status     string               `json:"status"` // eligible | unknown | ineligible | unextracted
	Reasons    []string             `json:"reasons,omitempty"`
	Unknown    []string             `json:"unknown,omitempty"`
	Extraction *extract.Eligibility `json:"extraction,omitempty"`
	Postings   int                  `json:"postings"` // postings merged into this cluster
}

// BuildJobs returns open internships with dedup clusters collapsed, freshest
// first. Shared by the live API and the static export.
func BuildJobs(ctx context.Context, st *store.Store, prof filter.Profile) ([]*jobJSON, error) {
	rows, err := st.Query(ctx, `is_intern=1 AND closed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	byCluster := map[string]*jobJSON{}
	out := []*jobJSON{}
	for _, row := range rows {
		cid := row.ClusterID
		if cid == "" {
			cid = row.Key()
		}
		if j, ok := byCluster[cid]; ok {
			j.Postings++
			j.Locations = appendUnique(j.Locations, row.Locations...)
			if row.FirstSeen.Before(j.FirstSeen) {
				j.FirstSeen = row.FirstSeen
			}
			continue
		}
		j := &jobJSON{
			Key: row.Key(), ATS: row.ATS, Company: row.Company, Title: row.Title,
			Locations: appendUnique(nil, row.Locations...), URL: row.URL, FirstSeen: row.FirstSeen,
			New: !row.Bootstrap, TitleMatch: prof.TitleMatches(row.Title) && prof.LocationMatches(row.Locations),
			Status: "unextracted", Postings: 1,
		}
		if !row.PostedAt.IsZero() {
			t := row.PostedAt
			j.PostedAt = &t
		}
		if row.Extraction != nil {
			v := prof.Check(*row.Extraction)
			j.Extraction, j.Reasons, j.Unknown = row.Extraction, v.Reasons, v.Unknown
			switch {
			case !v.Eligible:
				j.Status = "ineligible"
			case len(v.Unknown) > 0:
				j.Status = "unknown"
			default:
				j.Status = "eligible"
			}
		}
		byCluster[cid] = j
		out = append(out, j)
	}
	// Freshest first: ATS publish time when known, else when we first saw it.
	ts := func(j *jobJSON) time.Time {
		if j.PostedAt != nil {
			return *j.PostedAt
		}
		return j.FirstSeen
	}
	sort.SliceStable(out, func(a, b int) bool { return ts(out[a]).After(ts(out[b])) })
	return out, nil
}

// Snapshot is the static export consumed by the published page.
type Snapshot struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Boards      int        `json:"boards"`
	Jobs        []*jobJSON `json:"jobs"`
}

func BuildSnapshot(ctx context.Context, st *store.Store, prof filter.Profile) (Snapshot, error) {
	jobs, err := BuildJobs(ctx, st, prof)
	if err != nil {
		return Snapshot{}, err
	}
	runs, err := st.LatestRuns(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{GeneratedAt: time.Now(), Boards: len(runs), Jobs: jobs}, nil
}

func appendUnique(dst []string, xs ...string) []string {
	for _, x := range xs {
		dup := false
		for _, d := range dst {
			dup = dup || d == x
		}
		if !dup {
			dst = append(dst, x)
		}
	}
	return dst
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
