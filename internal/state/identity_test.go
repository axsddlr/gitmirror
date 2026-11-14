package state

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnershipIdentityRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, ok, err := s.OwnershipOf(ctx, "gt", "o/a"); err != nil || ok {
		t.Fatalf("OwnershipOf before mark = %v, %v", ok, err)
	}
	if err := s.MarkOwnedAs(ctx, "gt", "O/A", OwnedCreated, 42, "https://git.example"); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s.OwnershipOf(ctx, "gt", "o/a")
	if err != nil || !ok || rec != (OwnershipRecord{How: OwnedCreated, RepoID: 42, ForgeURL: "https://git.example"}) {
		t.Fatalf("OwnershipOf = %+v, %v, %v", rec, ok, err)
	}
	// Re-marking (adopting a replaced repository) overwrites every field.
	if err := s.MarkOwnedAs(ctx, "gt", "o/a", OwnedAdopted, 43, "https://git2.example"); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = s.OwnershipOf(ctx, "gt", "o/a")
	if rec != (OwnershipRecord{How: OwnedAdopted, RepoID: 43, ForgeURL: "https://git2.example"}) {
		t.Fatalf("after re-mark: %+v", rec)
	}
	// Owned keeps working on the same row.
	if how, ok, err := s.Owned(ctx, "gt", "o/a"); err != nil || !ok || how != OwnedAdopted {
		t.Fatalf("Owned = %v, %v, %v", how, ok, err)
	}
}

func TestOwnershipBackfillOnlyFillsUnset(t *testing.T) {
	s := openTemp(t)
	// A legacy row (MarkOwned has no identity).
	if err := s.MarkOwned(ctx, "gt", "o/a", OwnedCreated); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s.OwnershipOf(ctx, "gt", "o/a")
	if err != nil || !ok || rec.RepoID != 0 || rec.ForgeURL != "" {
		t.Fatalf("legacy row = %+v, %v, %v", rec, ok, err)
	}
	if err := s.BackfillOwnership(ctx, "gt", "O/a", 7, "https://x"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ = s.OwnershipOf(ctx, "gt", "o/a"); rec.RepoID != 7 || rec.ForgeURL != "https://x" || rec.How != OwnedCreated {
		t.Fatalf("after backfill: %+v", rec)
	}
	// Set values are never overwritten by a backfill.
	if err := s.BackfillOwnership(ctx, "gt", "o/a", 8, "https://y"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ = s.OwnershipOf(ctx, "gt", "o/a"); rec.RepoID != 7 || rec.ForgeURL != "https://x" {
		t.Fatalf("second backfill overwrote: %+v", rec)
	}
	// Backfilling an unowned repo creates nothing.
	if err := s.BackfillOwnership(ctx, "gt", "o/zzz", 9, "https://x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OwnershipOf(ctx, "gt", "o/zzz"); ok {
		t.Fatal("backfill created an ownership row")
	}
}

func TestMarkMissingObservedRestartsAfterGap(t *testing.T) {
	s := openTemp(t)
	clk := &manualClock{t: time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	k := key("a")
	gap := 2 * time.Hour
	t0 := clk.t
	if got, err := s.MarkMissingObserved(ctx, k, gap); err != nil || !got.Equal(t0) {
		t.Fatalf("first observation = %v, %v", got, err)
	}
	clk.t = clk.t.Add(90 * time.Minute)
	if got, _ := s.MarkMissingObserved(ctx, k, gap); !got.Equal(t0) {
		t.Fatalf("continuous observation restarted the clock: %v", got)
	}
	first, last, ok, err := s.MissingRecord(ctx, k)
	if err != nil || !ok || !first.Equal(t0) || !last.Equal(clk.t) {
		t.Fatalf("MissingRecord = %v %v %v %v", first, last, ok, err)
	}
	if err := s.SetMissingReported(ctx, k); err != nil {
		t.Fatal(err)
	}
	// 3h without an observation: the clock restarts and the reported flag
	// is reset (a new missing period).
	clk.t = clk.t.Add(3 * time.Hour)
	if got, _ := s.MarkMissingObserved(ctx, k, gap); !got.Equal(clk.t) {
		t.Fatalf("gap did not restart the clock: first_seen %v, want %v", got, clk.t)
	}
	if rep, _ := s.MissingReported(ctx, k); rep {
		t.Fatal("reported flag survived the restart")
	}
	// maxGap 0 never restarts (MarkMissing semantics).
	t1 := clk.t
	clk.t = clk.t.Add(100 * time.Hour)
	if got, _ := s.MarkMissingObserved(ctx, k, 0); !got.Equal(t1) {
		t.Fatalf("maxGap 0 restarted: %v", got)
	}
}

func TestMarkMissingObservedLegacyRowRestarts(t *testing.T) {
	s := openTemp(t)
	clk := &manualClock{t: time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	k := key("a")
	// A row written before last_seen existed.
	if _, err := s.db.Exec(`INSERT INTO missing(pair_key, first_seen) VALUES (?, ?)`, k.String(), clk.t.Add(-48*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MarkMissingObserved(ctx, k, 2*time.Hour); !got.Equal(clk.t) {
		t.Fatalf("legacy row without last_seen kept first_seen %v", got)
	}
}

func TestUnarchivedMarker(t *testing.T) {
	s := openTemp(t)
	k := key("a")
	if _, err := s.StartRun(ctx, k, "cli"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Pair(ctx, k); p.Unarchived {
		t.Fatal("marker set on a new pair")
	}
	if err := s.SetUnarchived(ctx, k, true); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Pair(ctx, k); !p.Unarchived {
		t.Fatal("marker not persisted")
	}
	if err := s.SetUnarchived(ctx, k, false); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Pair(ctx, k); p.Unarchived {
		t.Fatal("marker not cleared")
	}
	if err := s.SetUnarchived(ctx, key("nope"), true); err == nil {
		t.Fatal("SetUnarchived on an unknown pair succeeded")
	}
}

func TestMigration0006FromV5(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v5.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version > 5 {
			break
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 0)`, m.version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO ownership(dst_forge, dst_repo, how, created_at) VALUES ('gt', 'o/a', 'created', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, ok, err := s.OwnershipOf(ctx, "gt", "o/a")
	if err != nil || !ok || rec != (OwnershipRecord{How: OwnedCreated}) {
		t.Fatalf("upgraded ownership row = %+v, %v, %v", rec, ok, err)
	}
}
