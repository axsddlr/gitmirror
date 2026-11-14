package state

import (
	"testing"
	"time"
)

func TestAuditAppendAndListNewestFirst(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	a, b := key("a"), key("b")
	events := []Event{
		{Key: a, Action: AuditCreate, Detail: "created private repository"},
		{Key: b, Action: AuditAdopt, Detail: "adopted existing repository"},
		{Key: a, Action: AuditArchive, Detail: "source archived"},
		{Key: a, Action: AuditDelete, Detail: "source deleted"},
	}
	for _, ev := range events {
		if err := s.Audit(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.AuditLog(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("AuditLog = %+v, want 4 events", got)
	}
	for i, ev := range got {
		want := events[len(events)-1-i]
		if ev.Key != want.Key || ev.Action != want.Action || ev.Detail != want.Detail {
			t.Fatalf("event %d = %+v, want %+v", i, ev, want)
		}
		if ev.ID == 0 || ev.Time.IsZero() || ev.Time.Location() != time.UTC {
			t.Fatalf("event %d lacks id/time: %+v", i, ev)
		}
		if i > 0 && !ev.Time.Before(got[i-1].Time) {
			t.Fatalf("not newest first: %+v", got)
		}
	}
	lim, err := s.AuditLog(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lim) != 2 || lim[0].Action != AuditDelete || lim[1].Action != AuditArchive {
		t.Fatalf("AuditLog(2) = %+v", lim)
	}
}

func TestAuditRejectsUnknownAction(t *testing.T) {
	s := openTemp(t)
	if err := s.Audit(ctx, Event{Key: key("a"), Action: "rename"}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if got, _ := s.AuditLog(ctx, 0); len(got) != 0 {
		t.Fatalf("rejected event stored: %+v", got)
	}
}

func TestAuditSurvivesDeletePair(t *testing.T) {
	s := openTemp(t)
	k := key("a")
	id, err := s.StartRun(ctx, k, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id, RunOK, nil, "", "d", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, Event{Key: k, Action: AuditDelete, Detail: "destination deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePair(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditLog(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != k || got[0].Action != AuditDelete {
		t.Fatalf("audit after DeletePair = %+v", got)
	}
}

func TestMissingReported(t *testing.T) {
	s := openTemp(t)
	k := key("a")
	if ok, err := s.MissingReported(ctx, k); err != nil || ok {
		t.Fatalf("MissingReported without record = %v, %v", ok, err)
	}
	// Without a missing record there is nothing to flag.
	if err := s.SetMissingReported(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MissingReported(ctx, k); ok {
		t.Fatal("flag set without a missing record")
	}
	if _, err := s.MarkMissing(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MissingReported(ctx, k); ok {
		t.Fatal("fresh missing record already reported")
	}
	if err := s.SetMissingReported(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MissingReported(ctx, k); !ok {
		t.Fatal("flag not set")
	}
	// MarkMissing again keeps the flag; ClearMissing resets it.
	if _, err := s.MarkMissing(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MissingReported(ctx, k); !ok {
		t.Fatal("MarkMissing reset the flag")
	}
	if err := s.ClearMissing(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkMissing(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MissingReported(ctx, k); ok {
		t.Fatal("flag survived ClearMissing")
	}
}
