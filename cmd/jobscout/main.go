// Command jobscout finds new internship postings on public ATS job boards,
// extracts eligibility requirements with Claude, and sends alerts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Ann997943/jobscout/internal/bench"
	"github.com/Ann997943/jobscout/internal/eval"
	"github.com/Ann997943/jobscout/internal/events"
	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/filter"
	"github.com/Ann997943/jobscout/internal/model"
	"github.com/Ann997943/jobscout/internal/notify"
	"github.com/Ann997943/jobscout/internal/pipeline"
	"github.com/Ann997943/jobscout/internal/source"
	"github.com/Ann997943/jobscout/internal/store"
	"github.com/Ann997943/jobscout/internal/web"
)

const usage = `jobscout — internship finder for Greenhouse / Lever / Ashby boards

Usage: jobscout <command> [flags]

Commands:
  discover   build boards.yaml from the SimplifyJobs list (validates each board)
  poll       poll every board once: detect new/updated/closed postings, alert on new ones
  run        poll forever on an interval
  list       show tracked internships (optionally only eligible ones)
  serve      web dashboard + background polling (http://localhost:8080)
  export     write a static site (index.html + jobs.json) for GitHub Pages
  extract    backfill LLM eligibility extraction for open internships
  stats      board health and alert latency
  bench      coverage and lead time vs SimplifyJobs
  eval       sample postings for labeling / score extraction accuracy

Common flags: -db jobscout.db -boards boards.yaml -profile profile.yaml
Env: ANTHROPIC_API_KEY, DISCORD_WEBHOOK_URL, SLACK_WEBHOOK_URL, JOBSCOUT_MODEL
`

type common struct {
	db, boards, profile string
	concurrency         int
	noLLM               bool
	model               string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.db, "db", "jobscout.db", "SQLite database path")
	fs.StringVar(&c.boards, "boards", "boards.yaml", "boards file")
	fs.StringVar(&c.profile, "profile", "profile.yaml", "your profile (graduation, sponsorship, title keywords)")
	fs.IntVar(&c.concurrency, "concurrency", 8, "parallel requests")
	fs.BoolVar(&c.noLLM, "no-llm", false, "disable Claude extraction")
	fs.StringVar(&c.model, "model", envOr("JOBSCOUT_MODEL", "claude-opus-5-5"), "Claude model for extraction")
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type boardsFile struct {
	Boards []model.Board `yaml:"boards"`
}

func loadBoards(path string) ([]model.Board, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w (run `jobscout discover` to create it)", err)
	}
	var f boardsFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, bd := range f.Boards {
		if _, err := source.For(bd.ATS, nil); err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", path, i, err)
		}
		if f.Boards[i].Name == "" {
			f.Boards[i].Name = bd.Token
		}
	}
	return f.Boards, nil
}

func loadProfile(path string) filter.Profile {
	p, err := filter.LoadProfile(path)
	if err != nil {
		slog.Warn("no profile loaded; using no filters", "err", err)
	}
	return p
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "discover":
		err = cmdDiscover(ctx, args)
	case "poll":
		err = cmdPoll(ctx, args, false)
	case "run":
		err = cmdPoll(ctx, args, true)
	case "list":
		err = cmdList(ctx, args)
	case "serve":
		err = cmdServe(ctx, args)
	case "export":
		err = cmdExport(ctx, args)
	case "extract":
		err = cmdExtract(ctx, args)
	case "stats":
		err = cmdStats(ctx, args)
	case "bench":
		err = cmdBench(ctx, args)
	case "eval":
		err = cmdEval(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newPipeline(c common) (*pipeline.Pipeline, *store.Store, []model.Board, error) {
	boards, err := loadBoards(c.boards)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return nil, nil, nil, err
	}
	var ex *extract.Extractor
	if !c.noLLM {
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			slog.Warn("ANTHROPIC_API_KEY not set — LLM extraction disabled (use -no-llm to silence)")
		} else {
			ex = extract.New(c.model)
		}
	}
	bus := &events.MemoryBus{OnError: func(name string, e events.Event, err error) {
		slog.Warn("handler error", "handler", name, "job", e.Job.Key(), "err", err)
	}}
	p := &pipeline.Pipeline{
		Store: st, HTTP: source.NewClient(), Bus: bus, Profile: loadProfile(c.profile),
		Extractor: ex, Notifier: notify.FromEnv(), Concurrency: c.concurrency,
	}
	p.Init()
	return p, st, boards, nil
}

func cmdPoll(ctx context.Context, args []string, loop bool) error {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	var c common
	c.register(fs)
	interval := fs.Duration("interval", 10*time.Minute, "poll interval (run only)")
	fs.Parse(args)
	p, st, boards, err := newPipeline(c)
	if err != nil {
		return err
	}
	defer st.Close()
	for {
		stats := p.PollAll(ctx, boards)
		slog.Info("poll done", "summary", stats.String())
		if !loop || ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func cmdExtract(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	var c common
	c.register(fs)
	limit := fs.Int("limit", 50, "max postings to extract (0 = all)")
	fs.Parse(args)
	p, st, boards, err := newPipeline(c)
	if err != nil {
		return err
	}
	defer st.Close()
	if p.Extractor == nil {
		return fmt.Errorf("extraction needs ANTHROPIC_API_KEY")
	}
	done, failed := p.Backfill(ctx, boards, *limit)
	fmt.Printf("extracted %d, failed %d\n", done, failed)
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var c common
	c.register(fs)
	since := fs.Duration("since", 0, "only postings first seen within this window (e.g. 48h)")
	eligible := fs.Bool("eligible", false, "only postings that pass your profile (needs extraction)")
	all := fs.Bool("all", false, "ignore profile title/location filter")
	fs.Parse(args)
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	prof := loadProfile(c.profile)
	where := `is_intern=1 AND closed_at IS NULL`
	var qargs []any
	if *since > 0 {
		where += ` AND first_seen >= ?`
		qargs = append(qargs, time.Now().Add(-*since).Unix())
	}
	rows, err := st.Query(ctx, where+` ORDER BY first_seen DESC, company`, qargs...)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIRST SEEN\tCOMPANY\tTITLE\tLOCATION\tELIGIBILITY\tURL")
	n := 0
	seenCluster := map[string]bool{}
	for _, r := range rows {
		if !*all && (!prof.TitleMatches(r.Title) || !prof.LocationMatches(r.Locations)) {
			continue
		}
		if r.ClusterID != "" && seenCluster[r.ClusterID] {
			continue
		}
		seenCluster[r.ClusterID] = true
		elig := "—"
		if r.Extraction != nil {
			v := prof.Check(*r.Extraction)
			switch {
			case !v.Eligible:
				elig = "✗ " + strings.Join(v.Reasons, "; ")
			case len(v.Unknown) > 0:
				elig = "? " + strings.Join(v.Unknown, ", ")
			default:
				elig = "✓"
			}
			if *eligible && !v.Eligible {
				continue
			}
		} else if *eligible {
			continue
		}
		loc := r.LocationString()
		if len(loc) > 40 {
			loc = loc[:37] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.FirstSeen.Format("01-02 15:04"), r.Company, r.Title, loc, elig, r.URL)
		n++
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "%d postings (duplicates across locations collapsed)\n", n)
	return nil
}

func cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var c common
	c.register(fs)
	fs.Parse(args)
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	runs, err := st.LatestRuns(ctx)
	if err != nil {
		return err
	}
	failed := 0
	var slow []store.BoardHealth
	for _, r := range runs {
		if !r.LastOK {
			failed++
			fmt.Printf("FAIL %-45s %s\n", r.Board, r.LastErr)
		}
		slow = append(slow, r)
	}
	sort.Slice(slow, func(i, j int) bool { return slow[i].Duration > slow[j].Duration })
	fmt.Printf("\nboards: %d (failing: %d)\n", len(runs), failed)
	for i := 0; i < 5 && i < len(slow); i++ {
		fmt.Printf("  slowest: %-40s %s\n", slow[i].Board, slow[i].Duration)
	}
	all, err := st.Query(ctx, `1=1`)
	if err != nil {
		return err
	}
	var open, interns, extracted int
	var lags []time.Duration
	for _, r := range all {
		if r.ClosedAt == nil {
			open++
			if r.IsIntern {
				interns++
				if r.Extraction != nil {
					extracted++
				}
			}
		}
		// Detection latency: from the ATS publish time to our first sighting,
		// for postings that appeared after we started watching the board.
		if !r.Bootstrap && r.IsIntern && !r.PostedAt.IsZero() {
			lags = append(lags, r.FirstSeen.Sub(r.PostedAt))
		}
	}
	fmt.Printf("open postings: %d, open internships: %d (extracted: %d)\n", open, interns, extracted)
	if len(lags) > 0 {
		sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
		q := func(f float64) time.Duration { return lags[int(f*float64(len(lags)-1))].Round(time.Second) }
		fmt.Printf("detection latency (ATS publish → first seen), n=%d: p50=%s p90=%s max=%s\n", len(lags), q(.5), q(.9), lags[len(lags)-1].Round(time.Second))
	} else {
		fmt.Println("detection latency: no postings have appeared since bootstrap yet")
	}
	return nil
}

func cmdDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	var c common
	c.register(fs)
	src := fs.String("simplify", bench.DefaultSimplifyURL, "SimplifyJobs listings.json URL or path")
	days := fs.Int("days", 365, "only boards with listings posted in the last N days")
	fs.Parse(args)
	ls, err := bench.LoadListings(ctx, *src)
	if err != nil {
		return err
	}
	found := bench.DiscoverBoards(ctx, ls, time.Now().AddDate(0, 0, -*days), source.NewClient(), c.concurrency)
	// Merge with an existing file so hand-added boards are kept.
	existing, _ := loadBoards(c.boards)
	byID := map[string]model.Board{}
	for _, b := range existing {
		byID[strings.ToLower(b.ID())] = b
	}
	added := 0
	for _, b := range found {
		if _, ok := byID[strings.ToLower(b.ID())]; !ok {
			byID[strings.ToLower(b.ID())] = b
			added++
		}
	}
	var out []model.Board
	for _, b := range byID {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	data, err := yaml.Marshal(boardsFile{out})
	if err != nil {
		return err
	}
	header := "# Generated by `jobscout discover` from SimplifyJobs listings; safe to edit by hand.\n"
	if err := os.WriteFile(c.boards, append([]byte(header), data...), 0o644); err != nil {
		return err
	}
	fmt.Printf("%d valid boards discovered, %d new; %s now has %d boards\n", len(found), added, c.boards, len(out))
	return nil
}

func cmdBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var c common
	c.register(fs)
	src := fs.String("simplify", bench.DefaultSimplifyURL, "SimplifyJobs listings.json URL or path")
	missed := fs.Int("show-missed", 10, "print this many missed listings")
	fs.Parse(args)
	boards, err := loadBoards(c.boards)
	if err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ls, err := bench.LoadListings(ctx, *src)
	if err != nil {
		return err
	}
	rep, err := bench.Compare(ctx, st, ls, boards, loadProfile(c.profile))
	if err != nil {
		return err
	}
	rep.Print(os.Stdout, *missed)
	return nil
}

func cmdEval(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "sample" && args[0] != "run") {
		return fmt.Errorf("usage: jobscout eval sample -n 60 -out labels.jsonl | jobscout eval run -labels labels.jsonl")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("eval "+sub, flag.ExitOnError)
	var c common
	c.register(fs)
	n := fs.Int("n", 60, "sample size")
	seed := fs.Int64("seed", 1, "random seed")
	out := fs.String("out", "labels.jsonl", "output file (sample)")
	labels := fs.String("labels", "labels.jsonl", "labeled file (run)")
	pred := fs.String("pred", "eval_results.jsonl", "where to write predictions (run)")
	mism := fs.Int("show-mismatches", 20, "print this many mismatches")
	fs.Parse(args)

	if sub == "sample" {
		c.noLLM = true // descriptions only
		p, st, _, err := newPipeline(c)
		if err != nil {
			return err
		}
		defer st.Close()
		rows, err := st.Query(ctx, `is_intern=1 AND closed_at IS NULL`)
		if err != nil {
			return err
		}
		// Random, profile-relevant candidates (shuffle before fetching details
		// so the sample isn't biased toward whichever ATS sorts first).
		rng := rand.New(rand.NewSource(*seed))
		rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		prof := loadProfile(c.profile)
		kept := rows[:0]
		for _, r := range rows {
			if prof.TitleMatches(r.Title) {
				kept = append(kept, r)
			}
		}
		rows = kept
		// Make sure sampled candidates have descriptions (Greenhouse needs a detail fetch).
		var withDesc []store.Row
		for i := range rows {
			if rows[i].Description == "" {
				if _, _, err := p.Enrich(ctx, &rows[i]); err != nil {
					continue
				}
			}
			withDesc = append(withDesc, rows[i])
			if len(withDesc) >= *n*4 {
				break
			}
		}
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		k, err := eval.Sample(withDesc, *n, *seed, f)
		if err != nil {
			return err
		}
		fmt.Printf("wrote %d postings to %s — fill in each \"gold\" object and set \"labeled\": true\n", k, *out)
		return nil
	}

	items, err := eval.Load(*labels)
	if err != nil {
		return err
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return fmt.Errorf("eval run needs ANTHROPIC_API_KEY")
	}
	items = eval.Run(ctx, extract.New(c.model), items, c.concurrency)
	prof, perr := filter.LoadProfile(c.profile)
	var pp *filter.Profile
	if perr == nil {
		pp = &prof
	}
	res := eval.Score(items, pp)
	fmt.Printf("model: %s\n", c.model)
	res.Print(os.Stdout, *mism)
	if *pred != "" && *pred != *labels {
		f, err := os.Create(*pred)
		if err != nil {
			return err
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)
		for _, it := range items {
			if err := enc.Encode(it); err != nil {
				return err
			}
		}
		fmt.Printf("\npredictions written to %s\n", *pred)
	}
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	interval := fs.Duration("interval", 10*time.Minute, "background poll interval")
	noPoll := fs.Bool("no-poll", false, "dashboard only, no background polling")
	fs.Parse(args)
	p, st, boards, err := newPipeline(c)
	if err != nil {
		return err
	}
	defer st.Close()
	srv := &web.Server{Store: st, Profile: p.Profile, Boards: boards}
	if !*noPoll {
		srv.Pipeline = p
		go srv.PollLoop(ctx, *interval)
	}
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	slog.Info("dashboard listening", "url", "http://"+*addr)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var c common
	c.register(fs)
	dir := fs.String("dir", "site", "output directory (index.html + jobs.json)")
	fs.Parse(args)
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := web.BuildSnapshot(ctx, st, loadProfile(c.profile))
	if err != nil {
		return err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	page, err := web.IndexHTML()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "jobs.json"), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "index.html"), page, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d roles to %s/ (index.html, jobs.json %d KB)\n", len(snap.Jobs), *dir, len(b)/1024)
	return nil
}
