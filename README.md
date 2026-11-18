# jobscout

A bot that finds new internship postings within minutes of publication, reads each job description with an LLM to pull out eligibility requirements (graduation window, visa sponsorship, citizenship/clearance, degree level, on-site policy), drops the postings you don't qualify for, and sends the rest to Discord or Slack.

It only finds jobs. It never applies to them.

## Data sources

jobscout only calls the public, read-only job-board APIs that applicant tracking systems (ATS) publish so companies can embed their job boards:

| ATS | Endpoint | Notes |
|---|---|---|
| Greenhouse | `boards-api.greenhouse.io/v1/boards/{token}/jobs` | The list endpoint is lightweight and includes `updated_at`. The full JD is fetched only for internships that are new or changed. |
| Lever | `api.lever.co/v0/postings/{token}?mode=json` | Full descriptions are included in the list response. |
| Ashby | `api.ashbyhq.com/posting-api/job-board/{token}` | Full descriptions are included in the list response. |

jobscout does not scrape LinkedIn or Indeed. Scraping them violates their terms of service, and they block scrapers anyway.

## Architecture

```
boards.yaml ──► poller (N workers) ──► SyncBoard: diff vs SQLite ──► events bus
                                         new / updated / closed / reopened   │
                                                                               ▼
                         notify ◄── eligibility ◄── LLM extraction ◄── JD detail + dedup cluster
                  (Discord/Slack/stdout)  (profile.yaml)   (Claude, JSON schema)
```

- **Change detection.** Each source computes a `Version` from the cheapest signal it has: Greenhouse's `updated_at`, or a content hash for Lever and Ashby. Each poll diffs every board against the database in a single transaction and emits `new`, `updated`, `closed` or `reopened` events.
  - The first sync of a board is marked as a *bootstrap*. Postings that were already up at that point are tracked but never trigger alerts.
  - If a board returns an empty listing while it still has open postings, those postings are not closed. This guards against transient API failures.
  - Two board tokens can alias the same company board. When that happens, the posting is stored only once.
- **Dedup.** Many companies post the same role once per city, and title wording varies ("SWE Intern" vs "Software Engineering Internship – Summer 2027"). Titles are normalized: synonyms are mapped, and seasons, years and parentheticals are stripped. Postings at the same company whose token-set Jaccard similarity is at least 0.75 are put in one cluster. You get one alert per cluster, listing every location.
- **Internship detection.** A title regex runs first. The ATS's structured fields are also checked: Greenhouse custom fields, Lever `commitment` and Ashby `employmentType`. For example, Jane Street titles its interns "Software Engineer" and marks the internship only in the custom field `Employment Type = Summer Internship`.
- **LLM extraction.** Claude is called with structured outputs (`output_config.format`, a JSON schema), so the response always parses.
  - Every field defaults to `not_stated`. The model is told never to infer anything, and it must return a verbatim evidence quote for every field it fills.
  - Server-side refusal fallback is enabled.
- **Eligibility.** The extracted fields are compared against `profile.yaml`. A requirement the posting doesn't state never disqualifies a job. It is listed as unknown in the alert instead.
- **Event bus.** `events.Bus` is an interface. The in-process `MemoryBus` can be replaced with Kafka without touching the poller or the handlers. The roadmap below describes how.

## Quick start

```bash
go build -o bin/jobscout ./cmd/jobscout
cp profile.example.yaml profile.yaml        # set graduation date, sponsorship, title keywords

./bin/jobscout discover                     # builds boards.yaml from SimplifyJobs (~1,000 boards)
./bin/jobscout poll                         # first run = bootstrap (no alerts)

export ANTHROPIC_API_KEY=...                # enables eligibility extraction
export DISCORD_WEBHOOK_URL=...              # and/or SLACK_WEBHOOK_URL
./bin/jobscout run -interval 5m             # poll forever
./bin/jobscout serve -interval 5m           # same, plus a web dashboard at http://127.0.0.1:8080
./bin/jobscout export                       # static snapshot for the published page: site/index.html + site/jobs.json

./bin/jobscout list -eligible -since 48h    # what's new that I qualify for
./bin/jobscout extract -limit 100           # backfill extraction for existing postings
./bin/jobscout stats                        # board health, detection latency
./bin/jobscout bench                        # coverage and lead time vs SimplifyJobs
```

`JOBSCOUT_MODEL` sets the Claude model. The default is `claude-opus-5-5`; `claude-haiku-5-5` is much cheaper for bulk backfills.

## Deploying to GitHub Pages

`.github/workflows/pages.yml` runs every 15 minutes on GitHub Actions. Each run:
1. polls every board,
2. extracts eligibility for new postings (only when the `ANTHROPIC_API_KEY` secret is set),
3. exports `site/`, which holds `index.html` and `jobs.json`,
4. deploys `site/` to GitHub Pages.

The SQLite database is kept between runs in the Actions cache, so new/closed detection carries over from one run to the next.

To set it up:
1. In the repository settings, go to **Pages**, then **Source**, and choose **GitHub Actions**.
2. Optionally add these repository secrets: `ANTHROPIC_API_KEY`, `DISCORD_WEBHOOK_URL`, `SLACK_WEBHOOK_URL`.
3. Start the first run from the Actions tab with **Run workflow**.

On a public repository, GitHub pauses scheduled workflows after 60 days with no repository activity. Re-enable the workflow from the Actions tab if that happens.

## Measuring extraction quality

```bash
./bin/jobscout eval sample -n 80 -out labels.jsonl
# Label each posting by hand: fill in "gold" and set "labeled": true
./bin/jobscout eval run -labels labels.jsonl
```

The report shows:
- Per-field accuracy.
- The share of postings where every field is correct.
- End-to-end decision agreement for your profile, split into false excludes (an eligible job is hidden, which is the costly error) and false includes.

Predictions are written to `eval_results.jsonl` so you can inspect the errors. Fill in the gold labels before you look at any predictions, so the model's answers don't anchor your labels.

## Results so far (2026-11-11, first day)

| Metric | Value |
|---|---|
| Boards polled | 1,042 (Greenhouse 490, Ashby 389, Lever 163) |
| Full poll of all boards | 23–40 s with 16 workers |
| Open postings tracked | ~54,500, of which ~3,100 are internships |
| SimplifyJobs listings on boards we poll that are live on the company board | 1,133. jobscout fetched all 1,133 (100%). |
| …that jobscout classifies as internships | 98.2% |
| SimplifyJobs "active" listings already removed from the company board | 1.0% |
| Profile-matching internships jobscout found that SimplifyJobs doesn't list | 521 |
| First live alert | A posting found 2 minutes after the ATS published it |

Lead time against SimplifyJobs (`bench`) can only be measured for postings that appear after jobscout starts polling. Let `run` collect data for a week or two, then re-run `bench`.

Extraction accuracy has not been measured yet. It needs a hand-labeled set (see above).

## Project layout

```
cmd/jobscout/        CLI
internal/source/     Greenhouse / Lever / Ashby clients, ATS URL parser
internal/store/      SQLite schema, change detection (SyncBoard)
internal/events/     event types + Bus interface (MemoryBus)
internal/pipeline/   polling, enrichment, alert policy
internal/dedup/      title normalization + similarity
internal/filter/     internship detection, profile, eligibility rules
internal/extract/    Claude structured extraction
internal/notify/     Discord / Slack / stdout
internal/bench/      SimplifyJobs comparison + board discovery
internal/eval/       labeling template + accuracy scoring
```

## Roadmap

- **Kafka.** Implement `events.Bus` over Kafka with two topics: `job-events`, which pollers produce to, and `job-enriched`, which the extractor produces to. Consumer groups let extraction and notification scale independently. Keying messages by job key keeps per-posting ordering.
- **Kubernetes.** Run pollers as a Deployment, sharded by board-hash. Run the extractor as a Deployment with an HPA on consumer lag. Move SQLite to Postgres.
- **React dashboard.** A feed of eligible postings, plus a labeling UI for `eval`. The UI would show the JD on the left and the form on the right, replacing hand-edited JSONL.
- **Adaptive polling.** Poll boards that post often (and peak hiring months) more frequently, within each ATS's rate limits.
- **Smarter classification.** Use the LLM as a fallback classifier for ambiguous titles ("Fellow", "Resident", "Associate").
