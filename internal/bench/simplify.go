// Package bench compares jobscout against the community-maintained
// SimplifyJobs internship list, and discovers boards from it.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ann997943/jobscout/internal/dedup"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/source"
	"github.com/Ann997943/jobscout/internal/store"
)

const DefaultSimplifyURL = "https://raw.githubusercontent.com/SimplifyJobs/Summer2027-Internships/dev/.github/scripts/listings.json"

// unixTime accepts numbers or numeric strings.
type unixTime int64

func (u *unixTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*u = unixTime(int64(f))
	return nil
}

type Listing struct {
	ID          string   `json:"id"`
	Company     string   `json:"company_name"`
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Active      bool     `json:"active"`
	IsVisible   *bool    `json:"is_visible"`
	DatePosted  unixTime `json:"date_posted"`
	DateUpdated unixTime `json:"date_updated"`
	Terms       []string `json:"terms"`
	Locations   []string `json:"locations"`
	Sponsorship string   `json:"sponsorship"`
}

func (l Listing) Visible() bool { return l.IsVisible == nil || *l.IsVisible }
func (l Listing) Posted() time.Time {
	return time.Unix(int64(l.DatePosted), 0)
}

// LoadListings reads listings.json from a URL or local path.
func LoadListings(ctx context.Context, src string) ([]Listing, error) {
	var r io.Reader
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", src, resp.StatusCode)
		}
		r = resp.Body
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var ls []Listing
	if err := json.NewDecoder(r).Decode(&ls); err != nil {
		return nil, fmt.Errorf("decode listings: %w", err)
	}
	return ls, nil
}

// Report is the coverage / lead-time comparison.
type Report struct {
	SimplifyActive  int
	SimplifyOnATS   map[string]int
	OnTrackedBoards int // Simplify listings whose board jobscout polls
	// Of those:
	Live          int // posting is currently on the company's board
	LiveIntern    int // ...and jobscout classified it as an internship (would alert)
	NotIntern     []Listing
	StaleSimplify int // Simplify says active, but the posting is gone from the board
	Missed        []Listing

	OnlyUs           []store.Row
	LeadTimes        []time.Duration // simplify.date_posted - our first_seen (positive = jobscout first)
	LeadTimeExcluded int             // matched postings already up when jobscout bootstrapped
}

// Compare matches active Simplify listings against our DB.
func Compare(ctx context.Context, st *store.Store, listings []Listing, boards []model.Board, prof filter.Profile) (Report, error) {
	rep := Report{SimplifyOnATS: map[string]int{}}
	tracked := map[string]bool{}
	for _, b := range boards {
		tracked[b.ATS+":"+strings.ToLower(b.Token)] = true
	}
	rows, err := st.Query(ctx, `1=1`)
	if err != nil {
		return rep, err
	}
	byKey := map[string]store.Row{}
	byBoard := map[string][]store.Row{}
	for _, r := range rows {
		byKey[r.Key()] = r
		if r.ClosedAt == nil {
			bk := r.ATS + ":" + strings.ToLower(r.Board)
			byBoard[bk] = append(byBoard[bk], r)
		}
	}
	matchedKeys := map[string]bool{}

	for _, l := range listings {
		if !l.Active || !l.Visible() {
			continue
		}
		rep.SimplifyActive++
		p, ok := source.ParseATSURL(l.URL)
		if !ok {
			continue
		}
		rep.SimplifyOnATS[p.ATS]++
		r, found := byKey[p.ATS+":"+p.ExternalID]
		found = found && p.ExternalID != ""
		boardTracked := p.Board != "" && tracked[p.ATS+":"+strings.ToLower(p.Board)]
		if !boardTracked && !found {
			continue
		}
		rep.OnTrackedBoards++

		if !found && p.ExternalID == "" {
			// No posting id in the URL: fall back to title similarity on the board.
			for _, cand := range byBoard[p.ATS+":"+strings.ToLower(p.Board)] {
				if dedup.TitleSimilarity(cand.Title, l.Title) >= dedup.Threshold {
					r, found = cand, true
					break
				}
			}
			if !found {
				rep.Missed = append(rep.Missed, l)
				continue
			}
		}
		if !found || r.ClosedAt != nil {
			// We poll the whole board; a posting that isn't on it (or was
			// removed) is closed or unlisted, so Simplify's entry is stale.
			rep.StaleSimplify++
			continue
		}
		rep.Live++
		matchedKeys[r.Key()] = true
		if !r.IsIntern {
			rep.NotIntern = append(rep.NotIntern, l)
			continue
		}
		rep.LiveIntern++
		if r.Bootstrap {
			rep.LeadTimeExcluded++
		} else if l.DatePosted > 0 {
			rep.LeadTimes = append(rep.LeadTimes, l.Posted().Sub(r.FirstSeen))
		}
	}

	// Open internships we have that Simplify doesn't (restricted to the
	// profile's title filter, since Simplify only lists tech-ish roles).
	for _, r := range rows {
		if !r.IsIntern || r.ClosedAt != nil || matchedKeys[r.Key()] || !prof.TitleMatches(r.Title) {
			continue
		}
		rep.OnlyUs = append(rep.OnlyUs, r)
	}
	sort.Slice(rep.LeadTimes, func(i, j int) bool { return rep.LeadTimes[i] < rep.LeadTimes[j] })
	return rep, nil
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	return ds[int(q*float64(len(ds)-1))]
}

func (r Report) Print(w io.Writer, show int) {
	atsTotal := 0
	for _, n := range r.SimplifyOnATS {
		atsTotal += n
	}
	fmt.Fprintf(w, "SimplifyJobs active listings:             %d\n", r.SimplifyActive)
	fmt.Fprintf(w, "  on Greenhouse/Lever/Ashby:              %d  %v\n", atsTotal, r.SimplifyOnATS)
	fmt.Fprintf(w, "  on boards jobscout polls:               %d  (board coverage %s)\n", r.OnTrackedBoards, pct(r.OnTrackedBoards, atsTotal))
	fmt.Fprintf(w, "    stale in Simplify (gone from board):  %d  (%s)\n", r.StaleSimplify, pct(r.StaleSimplify, r.OnTrackedBoards))
	fmt.Fprintf(w, "    live on the board:                    %d\n", r.Live)
	fmt.Fprintf(w, "      fetched by jobscout:                %d  (%s)\n", r.Live, pct(r.Live, r.Live+len(r.Missed)))
	fmt.Fprintf(w, "      classified as internship:           %d  (classifier recall %s)\n", r.LiveIntern, pct(r.LiveIntern, r.Live))
	fmt.Fprintf(w, "    unmatched (no id, no title match):    %d\n", len(r.Missed))
	fmt.Fprintf(w, "Open internships only jobscout found:     %d  (matching your profile's title filter)\n", len(r.OnlyUs))
	fmt.Fprintf(w, "\nLead time vs SimplifyJobs (positive = jobscout first):\n")
	if len(r.LeadTimes) == 0 {
		fmt.Fprintf(w, "  no data yet — needs postings that appeared after jobscout started polling (%d matched postings were already up at bootstrap)\n", r.LeadTimeExcluded)
	} else {
		earlier := 0
		for _, d := range r.LeadTimes {
			if d > 0 {
				earlier++
			}
		}
		fmt.Fprintf(w, "  n=%d  median=%s  p25=%s  p75=%s  jobscout earlier in %s\n", len(r.LeadTimes),
			quantile(r.LeadTimes, .5).Round(time.Minute), quantile(r.LeadTimes, .25).Round(time.Minute),
			quantile(r.LeadTimes, .75).Round(time.Minute), pct(earlier, len(r.LeadTimes)))
	}
	list := func(title string, ls []Listing) {
		if show <= 0 || len(ls) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s (first %d of %d):\n", title, min(show, len(ls)), len(ls))
		for i, l := range ls {
			if i >= show {
				break
			}
			fmt.Fprintf(w, "  %s — %s\n    %s\n", l.Company, l.Title, l.URL)
		}
	}
	list("Live but not classified as internship", r.NotIntern)
	list("Unmatched", r.Missed)
}

// DiscoverBoards extracts ATS boards referenced by listings posted since
// `since`, then keeps those whose public API responds.
func DiscoverBoards(ctx context.Context, listings []Listing, since time.Time, c *source.Client, concurrency int) []model.Board {
	type cand struct {
		b     model.Board
		names map[string]int
	}
	cands := map[string]*cand{}
	for _, l := range listings {
		if l.Posted().Before(since) {
			continue
		}
		p, ok := source.ParseATSURL(l.URL)
		if !ok || p.Board == "" {
			continue
		}
		k := p.ATS + ":" + strings.ToLower(p.Board)
		if cands[k] == nil {
			cands[k] = &cand{b: model.Board{ATS: p.ATS, Token: p.Board}, names: map[string]int{}}
		}
		cands[k].names[l.Company]++
	}
	type result struct {
		b  model.Board
		ok bool
	}
	out := make(chan result)
	sem := make(chan struct{}, concurrency)
	for _, cd := range cands {
		best, n := "", 0
		for name, c := range cd.names {
			if c > n || (c == n && name < best) {
				best, n = name, c
			}
		}
		cd.b.Name = best
		go func(b model.Board) {
			sem <- struct{}{}
			defer func() { <-sem }()
			f, _ := source.For(b.ATS, c)
			_, err := f.List(ctx, b)
			out <- result{b, err == nil}
		}(cd.b)
	}
	var boards []model.Board
	for range cands {
		r := <-out
		if r.ok {
			boards = append(boards, r.b)
		}
	}
	sort.Slice(boards, func(i, j int) bool { return boards[i].ID() < boards[j].ID() })
	return boards
}
