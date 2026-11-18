package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ann997943/jobscout/internal/events"
	"github.com/Ann997943/jobscout/internal/model"
)

func job(id, title, version string) model.Job {
	return model.Job{ATS: "lever", Board: "acme", ExternalID: id, Company: "Acme", Title: title, URL: "u/" + id, Version: version, Description: "d" + version}
}

func types(evs []events.Event) map[string]events.Type {
	m := map[string]events.Type{}
	for _, e := range evs {
		m[e.Job.ExternalID] = e.Type
	}
	return m
}

func TestSyncBoardChangeDetection(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := model.Board{ATS: "lever", Token: "acme"}
	intern := func(model.Job) bool { return true }
	t0 := time.Unix(1_800_000_000, 0)

	evs, err := st.SyncBoard(ctx, b, []model.Job{job("1", "SWE Intern", "a"), job("2", "PM Intern", "a"), job("3", "x", "a"), job("4", "y", "a")}, intern, true, t0)
	if err != nil || len(evs) != 4 || !evs[0].Bootstrap {
		t.Fatalf("bootstrap: %v %+v", err, evs)
	}

	// 1 unchanged, 2 updated, 3+4 closed, 5 new
	evs, err = st.SyncBoard(ctx, b, []model.Job{job("1", "SWE Intern", "a"), job("2", "PM Intern", "b"), job("5", "Data Intern", "a")}, intern, false, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got := types(evs)
	want := map[string]events.Type{"2": events.Updated, "3": events.Closed, "4": events.Closed, "5": events.New}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("job %s: got %s want %s", k, got[k], v)
		}
	}

	// 3 comes back
	evs, _ = st.SyncBoard(ctx, b, []model.Job{job("1", "SWE Intern", "a"), job("2", "PM Intern", "b"), job("5", "Data Intern", "a"), job("3", "x", "a")}, intern, false, t0.Add(2*time.Minute))
	if types(evs)["3"] != events.Reopened {
		t.Errorf("want reopened, got %v", types(evs))
	}

	// Empty response must not close every job.
	evs, _ = st.SyncBoard(ctx, b, nil, intern, false, t0.Add(3*time.Minute))
	if len(evs) != 0 {
		t.Errorf("empty listing closed jobs: %v", types(evs))
	}

	r, err := st.Get(ctx, "lever:2")
	if err != nil || r.Description != "db" || r.Bootstrap != true {
		t.Errorf("row: %+v %v", r, err)
	}
}
