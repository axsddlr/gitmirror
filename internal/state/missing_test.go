package state

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// manualClock is a settable store clock.
type manualClock struct{ t time.Time }

func (c *manualClock) now() time.Time { return c.t }

func TestMarkMissingKeepsFirstSeen(t *testing.T) {
	s := openTemp(t)
	clk := &manualClock{t: time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)}
	s.SetClock(clk.now)
	k := key("a")

	if _, ok, err := s.MissingSince(ctx, k); err != nil || ok {
		t.Fatalf("MissingSince before mark = %v, %v", ok, err)
	}
	first := clk.t
	got, err := s.MarkMissing(ctx, k)
	if err != nil || !got.Equal(first) {
		t.Fatalf("MarkMissing = %v, %v; want %v", got, err, first)
	}
	clk.t = clk.t.Add(5 * time.Hour)
	got, err = s.MarkMissing(ctx, k)
	if err != nil || !got.Equal(first) {
		t.Fatalf("repeat MarkMissing = %v, %v; want original %v", got, err, first)
	}
	since, ok, err := s.MissingSince(ctx, k)
	if err != nil || !ok || !since.Equal(first) {
		t.Fatalf("MissingSince = %v, %v, %v; want %v", since, ok, err, first)
	}
	if _, ok, _ := s.MissingSince(ctx, key("b")); ok {
		t.Fatal("other pair reported missing")
	}

	if err := s.ClearMissing(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.MissingSince(ctx, k); ok {
		t.Fatal("still missing after ClearMissing")
	}
	if err := s.ClearMissing(ctx, k); err != nil {
		t.Fatalf("ClearMissing on absent record: %v", err)
	}
	// A fresh mark after clearing starts a new window.
	got, err = s.MarkMissing(ctx, k)
	if err != nil || !got.Equal(clk.t) {
		t.Fatalf("MarkMissing after clear = %v, %v; want %v", got, err, clk.t)
	}
}

func schemaVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func latestVersion(t *testing.T) int {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil || len(ms) == 0 {
		t.Fatalf("loadMigrations: %v (%d)", err, len(ms))
	}
	return ms[len(ms)-1].version
}

func TestMigrationFromV1(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v1.db")
	// Build a database exactly as a v1 binary would have left it.
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		string(v1),
		`INSERT INTO schema_migrations(version, applied_at) VALUES (1, 0)`,
		`INSERT INTO pairs(key, rule, src_forge, src_repo, dst_forge, dst_repo) VALUES ('k', 'r', 'gh', 'o/a', 'cb', 'o/a')`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("seed v1: %v", err)
		}
	}
	db.Close()

	s, err := Open(ctx, p)
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	latest := latestVersion(t)
	if latest < 3 {
		t.Fatalf("latest migration = %d, want >= 3 (asset map)", latest)
	}
	if v := schemaVersion(t, s); v != latest {
		t.Fatalf("schema version after upgrade = %d, want %d", v, latest)
	}
	if ps, _ := s.Pairs(ctx); len(ps) != 1 {
		t.Fatalf("pairs lost in migration: %+v", ps)
	}
	if _, err := s.MarkMissing(ctx, key("a")); err != nil {
		t.Fatalf("MarkMissing on upgraded db: %v", err)
	}
	if err := s.SetAssetMapping(ctx, key("a"), 5, 6); err != nil {
		t.Fatalf("SetAssetMapping on upgraded db: %v", err)
	}
	var n1 int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening is a no-op.
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n2 int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n1 != latest || n2 != latest || schemaVersion(t, s) != latest {
		t.Fatalf("migrations rows %d -> %d, version %d", n1, n2, schemaVersion(t, s))
	}
	if _, ok, _ := s.MissingSince(ctx, key("a")); !ok {
		t.Fatal("missing record lost on reopen")
	}
}

func TestDeletePairCascade(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	k, other := key("a"), key("b")
	for _, pk := range []PairKey{k, other} {
		id, err := s.StartRun(ctx, pk, "cli")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, id, RunOK, nil, "", "d", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.SetReleaseMapping(ctx, pk, 1, 10); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAssetMapping(ctx, pk, 100, 7); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkMissing(ctx, pk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkOwned(ctx, k.DstForge, k.DstRepo, OwnedCreated); err != nil {
		t.Fatal(err)
	}

	if err := s.DeletePair(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Pair(ctx, k); ok {
		t.Fatal("pair row survived DeletePair")
	}
	if runs, _ := s.Runs(ctx, k, 0); len(runs) != 0 {
		t.Fatalf("runs survived DeletePair: %+v", runs)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM runs WHERE pair_key = ?`, k.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d orphan run rows", n)
	}
	if m, _ := s.ReleaseMap(ctx, k); len(m) != 0 {
		t.Fatalf("release map survived DeletePair: %v", m)
	}
	if m, _ := s.AssetMap(ctx, k); len(m) != 0 {
		t.Fatalf("asset map survived DeletePair: %v", m)
	}
	if _, ok, _ := s.MissingSince(ctx, k); ok {
		t.Fatal("missing record survived DeletePair")
	}
	// Ownership is managed separately (ReleaseOwnership).
	if _, ok, _ := s.Owned(ctx, k.DstForge, k.DstRepo); !ok {
		t.Fatal("DeletePair touched ownership")
	}
	// The other pair is untouched.
	if _, ok, _ := s.Pair(ctx, other); !ok {
		t.Fatal("other pair deleted")
	}
	if runs, _ := s.Runs(ctx, other, 0); len(runs) != 1 {
		t.Fatalf("other runs = %d", len(runs))
	}
	if m, _ := s.ReleaseMap(ctx, other); m[1] != 10 {
		t.Fatal("other release map affected")
	}
	if m, _ := s.AssetMap(ctx, other); m[100] != 7 {
		t.Fatal("other asset map affected")
	}
	if _, ok, _ := s.MissingSince(ctx, other); !ok {
		t.Fatal("other missing record affected")
	}
	if err := s.DeletePair(ctx, k); err != nil {
		t.Fatalf("DeletePair on absent pair: %v", err)
	}
}
