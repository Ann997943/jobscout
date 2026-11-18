// Package eval measures extraction accuracy against hand-labeled postings.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/store"
)

// Item is one line of the labels JSONL file.
type Item struct {
	Key         string               `json:"key"`
	Company     string               `json:"company"`
	Title       string               `json:"title"`
	URL         string               `json:"url"`
	Locations   []string             `json:"locations"`
	Description string               `json:"description"`
	Labeled     bool                 `json:"labeled"` // set to true once you've filled in Gold
	Gold        extract.Eligibility  `json:"gold"`
	Pred        *extract.Eligibility `json:"pred,omitempty"`
	Error       string               `json:"error,omitempty"`
}

func (it Item) Job() model.Job {
	ats, id, _ := strings.Cut(it.Key, ":")
	return model.Job{ATS: ats, ExternalID: id, Company: it.Company, Title: it.Title, URL: it.URL, Locations: it.Locations, Description: it.Description}
}

// Sample writes n random internship postings (with descriptions) as an
// unlabeled template. Gold starts at "not stated" for every field.
func Sample(rows []store.Row, n int, seed int64, w io.Writer) (int, error) {
	var pool []store.Row
	for _, r := range rows {
		if strings.TrimSpace(r.Description) != "" {
			pool = append(pool, r)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if n > len(pool) {
		n = len(pool)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range pool[:n] {
		it := Item{Key: r.Key(), Company: r.Company, Title: r.Title, URL: r.URL, Locations: r.Locations, Description: r.Description,
			Gold: extract.Eligibility{Sponsorship: extract.NotStated, USCitizenshipRequired: extract.NotStated,
				ClearanceRequired: extract.NotStated, WorkMode: extract.NotStated, DegreeLevels: []string{}, Evidence: []extract.Evidence{}}}
		if err := enc.Encode(it); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func Load(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []Item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for ln := 1; sc.Scan(); ln++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var it Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, ln, err)
		}
		items = append(items, it)
	}
	return items, sc.Err()
}

// Fields compared, in report order.
var Fields = []string{"grad_earliest", "grad_latest", "degree_levels", "sponsorship",
	"us_citizenship_required", "security_clearance_required", "work_mode", "season"}

func fieldValue(e extract.Eligibility, f string) string {
	switch f {
	case "grad_earliest":
		return e.GradEarliest
	case "grad_latest":
		return e.GradLatest
	case "degree_levels":
		d := append([]string(nil), e.DegreeLevels...)
		sort.Strings(d)
		return strings.Join(d, ",")
	case "sponsorship":
		return e.Sponsorship
	case "us_citizenship_required":
		return e.USCitizenshipRequired
	case "security_clearance_required":
		return e.ClearanceRequired
	case "work_mode":
		return e.WorkMode
	case "season":
		return strings.ToLower(strings.Join(strings.Fields(e.Season), " "))
	}
	return ""
}

// Run predicts every labeled item with the extractor (concurrently).
func Run(ctx context.Context, ex *extract.Extractor, items []Item, concurrency int) []Item {
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := range items {
		if !items[i].Labeled {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it *Item) {
			defer wg.Done()
			defer func() { <-sem }()
			p, err := ex.Extract(ctx, it.Job())
			if err != nil {
				it.Error = err.Error()
				return
			}
			it.Pred = &p
		}(&items[i])
	}
	wg.Wait()
	return items
}

type Mismatch struct {
	Key, Title, Field, Gold, Pred string
}

type Result struct {
	N, Errors      int
	Correct        map[string]int
	Mismatches     []Mismatch
	ExactAll       int // items with every field correct
	DecisionAgree  int // profile verdict identical under gold vs pred
	FalseExclude   int // gold says eligible, pred says not (you'd miss a job)
	FalseInclude   int // gold says ineligible, pred says eligible (noise)
	ProfileChecked bool
}

// Score compares predictions with gold labels. If prof is non-nil it also
// reports end-to-end filter decision agreement for that profile.
func Score(items []Item, prof *filter.Profile) Result {
	r := Result{Correct: map[string]int{}, ProfileChecked: prof != nil}
	for _, it := range items {
		if !it.Labeled {
			continue
		}
		if it.Pred == nil {
			r.Errors++
			continue
		}
		r.N++
		all := true
		for _, f := range Fields {
			g, p := fieldValue(it.Gold, f), fieldValue(*it.Pred, f)
			if g == p {
				r.Correct[f]++
			} else {
				all = false
				r.Mismatches = append(r.Mismatches, Mismatch{it.Key, it.Title, f, g, p})
			}
		}
		if all {
			r.ExactAll++
		}
		if prof != nil {
			gv, pv := prof.Check(it.Gold).Eligible, prof.Check(*it.Pred).Eligible
			switch {
			case gv == pv:
				r.DecisionAgree++
			case gv && !pv:
				r.FalseExclude++
			default:
				r.FalseInclude++
			}
		}
	}
	return r
}

func (r Result) Print(w io.Writer, showMismatches int) {
	fmt.Fprintf(w, "Labeled items scored: %d (errors: %d)\n\n", r.N, r.Errors)
	fmt.Fprintf(w, "%-30s %8s\n", "field", "accuracy")
	for _, f := range Fields {
		fmt.Fprintf(w, "%-30s %7.1f%%\n", f, pct(r.Correct[f], r.N))
	}
	fmt.Fprintf(w, "%-30s %7.1f%%\n", "ALL FIELDS CORRECT", pct(r.ExactAll, r.N))
	if r.ProfileChecked {
		fmt.Fprintf(w, "\nEligibility decision for your profile:\n")
		fmt.Fprintf(w, "  agreement      %5.1f%%\n", pct(r.DecisionAgree, r.N))
		fmt.Fprintf(w, "  false exclude  %d  (eligible job wrongly filtered out — the costly error)\n", r.FalseExclude)
		fmt.Fprintf(w, "  false include  %d  (ineligible job let through)\n", r.FalseInclude)
	}
	if showMismatches > 0 && len(r.Mismatches) > 0 {
		fmt.Fprintf(w, "\nMismatches (first %d):\n", showMismatches)
		for i, m := range r.Mismatches {
			if i >= showMismatches {
				break
			}
			fmt.Fprintf(w, "  %-28s %-24s gold=%q pred=%q  (%s)\n", m.Field, m.Key, m.Gold, m.Pred, m.Title)
		}
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
