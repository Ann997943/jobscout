// Package store persists jobs in SQLite and performs change detection.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Ann997943/jobscout/internal/events"
	"github.com/Ann997943/jobscout/internal/extract"
	"github.com/Ann997943/jobscout/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  key           TEXT PRIMARY KEY,
  ats           TEXT NOT NULL,
  board         TEXT NOT NULL,
  external_id   TEXT NOT NULL,
  company       TEXT NOT NULL,
  title         TEXT NOT NULL,
  locations     TEXT NOT NULL DEFAULT '[]',
  url           TEXT NOT NULL,
  description   TEXT NOT NULL DEFAULT '',
  version       TEXT NOT NULL,
  desc_version  TEXT NOT NULL DEFAULT '',
  posted_at     INTEGER,
  first_seen    INTEGER NOT NULL,
  last_seen     INTEGER NOT NULL,
  changed_at    INTEGER NOT NULL,
  closed_at     INTEGER,
  bootstrap     INTEGER NOT NULL DEFAULT 0,
  is_intern     INTEGER NOT NULL DEFAULT 0,
  cluster_id    TEXT NOT NULL DEFAULT '',
  extraction    TEXT,
  extracted_version TEXT NOT NULL DEFAULT '',
  notified_at   INTEGER
);
CREATE INDEX IF NOT EXISTS jobs_board ON jobs(ats, board);
CREATE INDEX IF NOT EXISTS jobs_cluster ON jobs(cluster_id);
CREATE INDEX IF NOT EXISTS jobs_intern_open ON jobs(is_intern, closed_at);

CREATE TABLE IF NOT EXISTS events (
  id    INTEGER PRIMARY KEY AUTOINCREMENT,
  key   TEXT NOT NULL,
  type  TEXT NOT NULL,
  at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS board_runs (
  board       TEXT NOT NULL,
  run_at      INTEGER NOT NULL,
  ok          INTEGER NOT NULL,
  n_jobs      INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  error       TEXT
);
CREATE INDEX IF NOT EXISTS board_runs_board ON board_runs(board, run_at);
`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps things simple and safe
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Row is a stored job plus tracking metadata.
type Row struct {
	model.Job
	DescVersion      string
	FirstSeen        time.Time
	LastSeen         time.Time
	ClosedAt         *time.Time
	Bootstrap        bool
	IsIntern         bool
	ClusterID        string
	Extraction       *extract.Eligibility
	ExtractedVersion string
	NotifiedAt       *time.Time
}

// BoardSeen reports whether a board has been synced successfully before.
func (s *Store) BoardSeen(ctx context.Context, boardID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM board_runs WHERE board=? AND ok=1`, boardID).Scan(&n)
	return n > 0, err
}

func (s *Store) RecordRun(ctx context.Context, boardID string, at time.Time, nJobs int, dur time.Duration, runErr error) error {
	var msg any
	if runErr != nil {
		msg = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO board_runs(board, run_at, ok, n_jobs, duration_ms, error) VALUES(?,?,?,?,?,?)`,
		boardID, at.Unix(), btoi(runErr == nil), nJobs, dur.Milliseconds(), msg)
	return err
}

// SyncBoard diffs a fresh listing of one board against the database inside a
// single transaction and returns the resulting change events.
//
// isIntern classifies postings; bootstrap marks the board's first ever sync.
func (s *Store) SyncBoard(ctx context.Context, b model.Board, jobs []model.Job, isIntern func(model.Job) bool, bootstrap bool, now time.Time) ([]events.Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type existing struct {
		version string
		closed  bool
		row     model.Job
	}
	have := map[string]existing{}
	rows, err := tx.QueryContext(ctx, `SELECT key, version, closed_at IS NOT NULL, external_id, company, title, locations, url FROM jobs WHERE ats=? AND board=?`, b.ATS, b.Token)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v, locs string
		var closed bool
		j := model.Job{ATS: b.ATS, Board: b.Token}
		if err := rows.Scan(&k, &v, &closed, &j.ExternalID, &j.Company, &j.Title, &locs, &j.URL); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(locs), &j.Locations)
		have[k] = existing{v, closed, j}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var evs []events.Event
	seen := map[string]bool{}
	ts := now.Unix()
	for _, j := range jobs {
		k := j.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		intern := isIntern(j)
		desc, descVersion := "", ""
		if intern && j.Description != "" {
			desc, descVersion = j.Description, j.Version
		}
		locs, _ := json.Marshal(j.Locations)
		var posted any
		if !j.PostedAt.IsZero() {
			posted = j.PostedAt.Unix()
		}

		ex, ok := have[k]
		var typ events.Type
		switch {
		case !ok:
			// ON CONFLICT: two board tokens can alias the same company board
			// (posting ids are unique per ATS), so the first board owns the row.
			var res sql.Result
			res, err = tx.ExecContext(ctx, `INSERT INTO jobs(key, ats, board, external_id, company, title, locations, url, description, version, desc_version, posted_at, first_seen, last_seen, changed_at, bootstrap, is_intern)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key) DO NOTHING`,
				k, j.ATS, j.Board, j.ExternalID, j.Company, j.Title, string(locs), j.URL, desc, j.Version, descVersion, posted, ts, ts, ts, btoi(bootstrap), btoi(intern))
			if err == nil {
				if n, _ := res.RowsAffected(); n == 1 {
					typ = events.New
				}
			}
		case ex.closed:
			typ = events.Reopened
		case ex.version != j.Version:
			typ = events.Updated
		default:
			// Re-classify too, so improving the internship rules applies to old rows.
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET last_seen=?, is_intern=? WHERE key=?`, ts, btoi(intern), k)
		}
		if err != nil {
			return nil, err
		}
		if typ == events.Reopened || typ == events.Updated {
			// Keep an existing description if this source's list endpoint has none;
			// desc_version tells the pipeline it is stale.
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET company=?, title=?, locations=?, url=?, version=?, posted_at=COALESCE(?, posted_at),
				description=CASE WHEN ?<>'' THEN ? ELSE description END,
				desc_version=CASE WHEN ?<>'' THEN ? ELSE desc_version END,
				last_seen=?, changed_at=?, closed_at=NULL, is_intern=? WHERE key=?`,
				j.Company, j.Title, string(locs), j.URL, j.Version, posted, desc, desc, descVersion, descVersion, ts, ts, btoi(intern), k)
			if err != nil {
				return nil, err
			}
		}
		if typ != "" {
			evs = append(evs, events.Event{Type: typ, Job: j, At: now, Bootstrap: bootstrap})
		}
	}

	// Guard against a transient empty response wiping out a whole board.
	open := 0
	for _, ex := range have {
		if !ex.closed {
			open++
		}
	}
	if !(len(jobs) == 0 && open > 3) {
		for k, ex := range have {
			if !ex.closed && !seen[k] {
				if _, err := tx.ExecContext(ctx, `UPDATE jobs SET closed_at=?, changed_at=? WHERE key=?`, ts, ts, k); err != nil {
					return nil, err
				}
				evs = append(evs, events.Event{Type: events.Closed, Job: ex.row, At: now})
			}
		}
	}

	for _, e := range evs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(key, type, at) VALUES(?,?,?)`, e.Job.Key(), string(e.Type), ts); err != nil {
			return nil, err
		}
	}
	return evs, tx.Commit()
}

func (s *Store) SetDescription(ctx context.Context, key, desc, version string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET description=?, desc_version=? WHERE key=?`, desc, version, key)
	return err
}

func (s *Store) SetExtraction(ctx context.Context, key string, e extract.Eligibility, version string) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE jobs SET extraction=?, extracted_version=? WHERE key=?`, string(b), version, key)
	return err
}

func (s *Store) SetCluster(ctx context.Context, key, cluster string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET cluster_id=? WHERE key=?`, cluster, key)
	return err
}

func (s *Store) MarkNotified(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET notified_at=? WHERE key=?`, at.Unix(), key)
	return err
}

// ClusterNotified reports whether any posting in the cluster was already sent.
func (s *Store) ClusterNotified(ctx context.Context, cluster string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE cluster_id=? AND (notified_at IS NOT NULL OR bootstrap=1)`, cluster).Scan(&n)
	return n > 0, err
}

const rowCols = `key, ats, board, external_id, company, title, locations, url, description, version, desc_version,
  posted_at, first_seen, last_seen, closed_at, bootstrap, is_intern, cluster_id, extraction, extracted_version, notified_at`

func scanRow(sc interface{ Scan(...any) error }) (Row, error) {
	var r Row
	var key, locs string
	var posted, closed, notified sql.NullInt64
	var first, last int64
	var ext sql.NullString
	err := sc.Scan(&key, &r.ATS, &r.Board, &r.ExternalID, &r.Company, &r.Title, &locs, &r.URL, &r.Description, &r.Version, &r.DescVersion,
		&posted, &first, &last, &closed, &r.Bootstrap, &r.IsIntern, &r.ClusterID, &ext, &r.ExtractedVersion, &notified)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(locs), &r.Locations)
	if posted.Valid {
		r.PostedAt = time.Unix(posted.Int64, 0)
	}
	r.FirstSeen, r.LastSeen = time.Unix(first, 0), time.Unix(last, 0)
	if closed.Valid {
		t := time.Unix(closed.Int64, 0)
		r.ClosedAt = &t
	}
	if notified.Valid {
		t := time.Unix(notified.Int64, 0)
		r.NotifiedAt = &t
	}
	if ext.Valid {
		var e extract.Eligibility
		if json.Unmarshal([]byte(ext.String), &e) == nil {
			r.Extraction = &e
		}
	}
	return r, nil
}

func (s *Store) Get(ctx context.Context, key string) (Row, error) {
	return scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowCols+` FROM jobs WHERE key=?`, key))
}

// Query lists jobs. where is a trusted SQL fragment built by callers in this
// repo (never user input); args are bound parameters.
func (s *Store) Query(ctx context.Context, where string, args ...any) ([]Row, error) {
	q := `SELECT ` + rowCols + ` FROM jobs`
	if strings.TrimSpace(where) != "" {
		q += ` WHERE ` + where
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenInternsAtCompany returns open internship rows whose company name
// matches (case-insensitively); used for dedup clustering.
func (s *Store) OpenInternsAtCompany(ctx context.Context, company string) ([]Row, error) {
	return s.Query(ctx, `is_intern=1 AND closed_at IS NULL AND lower(company)=lower(?)`, company)
}

// Stats used by the `stats` command.
type BoardHealth struct {
	Board    string
	LastRun  time.Time
	LastOK   bool
	LastErr  string
	Duration time.Duration
}

func (s *Store) LatestRuns(ctx context.Context) ([]BoardHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.board, r.run_at, r.ok, COALESCE(r.error,''), r.duration_ms FROM board_runs r
		JOIN (SELECT board, MAX(run_at) m FROM board_runs GROUP BY board) x ON x.board=r.board AND x.m=r.run_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardHealth
	for rows.Next() {
		var h BoardHealth
		var at, ms int64
		if err := rows.Scan(&h.Board, &at, &h.LastOK, &h.LastErr, &ms); err != nil {
			return nil, err
		}
		h.LastRun, h.Duration = time.Unix(at, 0), time.Duration(ms)*time.Millisecond
		out = append(out, h)
	}
	return out, rows.Err()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
