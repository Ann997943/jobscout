// Package pipeline wires polling, change detection, enrichment and alerts.
//
//	poll boards ──► SyncBoard (diff) ──► events bus ──► enrich: JD detail, dedup cluster,
//	                                                     LLM extraction, eligibility ──► notify
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Ann997943/jobscout/internal/dedup"
	"github.com/Ann997943/jobscout/internal/events"
	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/notify"
	"github.com/Ann997943/jobscout/internal/source"
	"github.com/Ann997943/jobscout/internal/store"
)

type Pipeline struct {
	Store       *store.Store
	HTTP        *source.Client
	Bus         events.Bus
	Profile     filter.Profile
	Extractor   *extract.Extractor // nil disables LLM extraction
	Notifier    notify.Notifier
	Concurrency int
	Log         *slog.Logger
	Now         func() time.Time

	boards map[string]model.Board
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Init registers the bus subscribers. Call once.
func (p *Pipeline) Init() {
	if p.Concurrency <= 0 {
		p.Concurrency = 8
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	p.Bus.Subscribe("enrich-notify", p.handle)
}

type PollStats struct {
	Boards, Failed int
	Events         map[events.Type]int
	Took           time.Duration
}

func (s PollStats) String() string {
	return fmt.Sprintf("%d boards (%d failed) in %s — new=%d updated=%d closed=%d reopened=%d",
		s.Boards, s.Failed, s.Took.Round(time.Millisecond), s.Events[events.New], s.Events[events.Updated], s.Events[events.Closed], s.Events[events.Reopened])
}

// PollAll polls every board once with bounded concurrency.
func (p *Pipeline) PollAll(ctx context.Context, boards []model.Board) PollStats {
	start := p.now()
	p.boards = map[string]model.Board{}
	for _, b := range boards {
		p.boards[b.ATS+":"+b.Token] = b
	}
	st := PollStats{Boards: len(boards), Events: map[events.Type]int{}}
	var mu sync.Mutex
	sem := make(chan struct{}, p.Concurrency)
	var wg sync.WaitGroup
	for _, b := range boards {
		wg.Add(1)
		sem <- struct{}{}
		go func(b model.Board) {
			defer wg.Done()
			defer func() { <-sem }()
			evs, err := p.pollBoard(ctx, b)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				st.Failed++
				p.Log.Warn("poll failed", "board", b.ID(), "err", err)
				return
			}
			for _, e := range evs {
				st.Events[e.Type]++
			}
		}(b)
	}
	wg.Wait()
	st.Took = p.now().Sub(start)
	return st
}

func (p *Pipeline) pollBoard(ctx context.Context, b model.Board) ([]events.Event, error) {
	f, err := source.For(b.ATS, p.HTTP)
	if err != nil {
		return nil, err
	}
	seen, err := p.Store.BoardSeen(ctx, b.ID())
	if err != nil {
		return nil, err
	}
	t0 := p.now()
	jobs, err := f.List(ctx, b)
	var evs []events.Event
	if err == nil {
		isIntern := filter.IsInternshipJob
		evs, err = p.Store.SyncBoard(ctx, b, jobs, isIntern, !seen, p.now())
	}
	_ = p.Store.RecordRun(ctx, b.ID(), t0, len(jobs), p.now().Sub(t0), err)
	if err != nil {
		return nil, err
	}
	if !seen {
		p.Log.Info("bootstrapped board", "board", b.ID(), "jobs", len(jobs))
	}
	return evs, p.Bus.Publish(ctx, evs...)
}

// handle enriches internship events and sends alerts for genuinely new ones.
func (p *Pipeline) handle(ctx context.Context, e events.Event) error {
	if e.Type == events.Closed || !filter.IsInternshipJob(e.Job) {
		return nil
	}
	row, err := p.Store.Get(ctx, e.Job.Key())
	if err != nil {
		return err
	}
	if row.ClusterID == "" {
		if err := p.assignCluster(ctx, &row); err != nil {
			return err
		}
	}
	if e.Bootstrap {
		return nil // existing postings: tracked, but not news
	}
	if !p.Profile.TitleMatches(row.Title) || !p.Profile.LocationMatches(row.Locations) {
		return nil
	}
	ext, verdict, err := p.Enrich(ctx, &row)
	if err != nil {
		p.Log.Warn("enrich failed; alerting without extraction", "job", row.Key(), "err", err)
	}
	if verdict != nil && !verdict.Eligible {
		p.Log.Info("skipped: not eligible", "job", row.Key(), "title", row.Title, "reasons", verdict.Reasons)
		return nil
	}
	if e.Type == events.Updated {
		return nil // only alert on new/reopened postings
	}
	already, err := p.Store.ClusterNotified(ctx, row.ClusterID)
	if err != nil || already {
		return err
	}
	others, _ := p.Store.Query(ctx, `cluster_id=? AND key<>? AND closed_at IS NULL`, row.ClusterID, row.Key())
	var otherLocs []string
	for _, o := range others {
		otherLocs = append(otherLocs, o.Locations...)
	}
	now := p.now()
	if err := p.Notifier.Send(ctx, notify.Alert{Job: row.Job, OtherLocs: otherLocs, Extraction: ext, Verdict: verdict, FoundAt: now}); err != nil {
		return err
	}
	return p.Store.MarkNotified(ctx, row.Key(), now)
}

// assignCluster puts the posting in the cluster of an existing posting for
// the same role at the same company (e.g. one requisition per city).
func (p *Pipeline) assignCluster(ctx context.Context, row *store.Row) error {
	peers, err := p.Store.OpenInternsAtCompany(ctx, row.Company)
	if err != nil {
		return err
	}
	cluster, best := row.Key(), 0.0
	for _, o := range peers {
		if o.Key() == row.Key() || o.ClusterID == "" {
			continue
		}
		if s := dedup.TitleSimilarity(row.Title, o.Title); s >= dedup.Threshold && s > best {
			cluster, best = o.ClusterID, s
		}
	}
	row.ClusterID = cluster
	return p.Store.SetCluster(ctx, row.Key(), cluster)
}

// Enrich makes sure the row has a current description and extraction, then
// checks eligibility. Returns (nil, nil, nil) when extraction is disabled.
func (p *Pipeline) Enrich(ctx context.Context, row *store.Row) (*extract.Eligibility, *filter.Verdict, error) {
	if row.Description == "" || row.DescVersion != row.Version {
		b, ok := p.boards[row.ATS+":"+row.Board]
		if !ok {
			b = model.Board{ATS: row.ATS, Token: row.Board, Name: row.Company}
		}
		f, err := source.For(row.ATS, p.HTTP)
		if err != nil {
			return nil, nil, err
		}
		full, err := f.Detail(ctx, b, row.Job)
		if err != nil {
			return nil, nil, fmt.Errorf("detail: %w", err)
		}
		if full.Description != "" {
			row.Description, row.DescVersion = full.Description, row.Version
			if err := p.Store.SetDescription(ctx, row.Key(), full.Description, row.Version); err != nil {
				return nil, nil, err
			}
		}
	}
	if p.Extractor == nil {
		return nil, nil, nil
	}
	if row.Extraction == nil || row.ExtractedVersion != row.Version {
		e, err := p.Extractor.Extract(ctx, row.Job)
		if err != nil {
			if errors.Is(err, extract.ErrRefused) {
				p.Log.Warn("extraction refused", "job", row.Key())
			}
			return nil, nil, err
		}
		row.Extraction, row.ExtractedVersion = &e, row.Version
		if err := p.Store.SetExtraction(ctx, row.Key(), e, row.Version); err != nil {
			return nil, nil, err
		}
	}
	v := p.Profile.Check(*row.Extraction)
	return row.Extraction, &v, nil
}

// Backfill extracts eligibility for open internships that match the profile's
// title/location filters but have no (current) extraction yet.
func (p *Pipeline) Backfill(ctx context.Context, boards []model.Board, limit int) (done, failed int) {
	p.boards = map[string]model.Board{}
	for _, b := range boards {
		p.boards[b.ATS+":"+b.Token] = b
	}
	rows, err := p.Store.Query(ctx, `is_intern=1 AND closed_at IS NULL AND (extraction IS NULL OR extracted_version<>version) ORDER BY first_seen DESC`)
	if err != nil {
		p.Log.Error("backfill query", "err", err)
		return 0, 0
	}
	var todo []store.Row
	for _, r := range rows {
		if p.Profile.TitleMatches(r.Title) && p.Profile.LocationMatches(r.Locations) {
			todo = append(todo, r)
		}
		if limit > 0 && len(todo) >= limit {
			break
		}
	}
	var mu sync.Mutex
	sem := make(chan struct{}, p.Concurrency)
	var wg sync.WaitGroup
	for i := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(r store.Row) {
			defer wg.Done()
			defer func() { <-sem }()
			_, _, err := p.Enrich(ctx, &r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				p.Log.Warn("backfill", "job", r.Key(), "err", err)
				return
			}
			done++
			if (done+failed)%25 == 0 {
				p.Log.Info("backfill progress", "done", done, "failed", failed, "total", len(todo))
			}
		}(todo[i])
	}
	wg.Wait()
	return done, failed
}
