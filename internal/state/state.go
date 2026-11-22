// Package state persists gitmirror's durable safety state in SQLite:
// destination ownership, per-pair last-known digests, run history, the
// release id mapping, when a pair's source was first seen missing, an
// append-only audit log of destination create/adopt/archive/delete actions,
// and the forges, rules and settings managed in the web UI (see ui.go).
//
// Times are stored as int64 unix nanoseconds (UTC); 0 means "unset" and is
// returned as the zero time.Time. Nanosecond storage round-trips exactly.
//
// The database uses a single connection (SetMaxOpenConns(1)) so all access is
// serialized inside the process, which avoids SQLITE_BUSY between goroutines;
// busy_timeout=5000 additionally covers other processes.
package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite" // pure-Go sqlite driver ("sqlite")
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// PairKey identifies one concrete source repo to destination repo pair under
// a rule. Repos are "owner/name".
type PairKey struct {
	Rule, SrcForge, SrcRepo, DstForge, DstRepo string
}

// String returns a stable, unambiguous encoding (a JSON array of the five
// parts) suitable as a database key.
func (k PairKey) String() string {
	b, _ := json.Marshal([]string{k.Rule, k.SrcForge, k.SrcRepo, k.DstForge, k.DstRepo})
	return string(b)
}

// Ownership records how a destination repo came to be managed by the tool.
type Ownership string

// Ownership values.
const (
	// OwnedCreated means the tool created the destination repo.
	OwnedCreated Ownership = "created"
	// OwnedAdopted means an existing destination repo was adopted via the rule.
	OwnedAdopted Ownership = "adopted"
)

// StepStatus is the outcome of one sync step.
type StepStatus string

// StepStatus values.
const (
	// StepOK means the step ran and succeeded.
	StepOK StepStatus = "ok"
	// StepSkipped means the step was not applicable or disabled.
	StepSkipped StepStatus = "skipped"
	// StepFailed means the step failed.
	StepFailed StepStatus = "failed"
	// StepUnchanged means the step found nothing to do.
	StepUnchanged StepStatus = "unchanged"
)

// StepResult is the recorded result of one step (git, lfs, settings, ...).
type StepResult struct {
	Step     string
	Status   StepStatus
	Detail   string
	Duration time.Duration
}

// RunStatus is the lifecycle/outcome state of a run.
type RunStatus string

// RunStatus values.
const (
	// RunRunning means the run has started and not finished.
	RunRunning RunStatus = "running"
	// RunOK means all steps succeeded.
	RunOK RunStatus = "ok"
	// RunPartial means some steps failed but the core sync succeeded.
	RunPartial RunStatus = "partial"
	// RunFailed means the run failed.
	RunFailed RunStatus = "failed"
	// RunAborted means a safety rule or shutdown stopped the run.
	RunAborted RunStatus = "aborted"
)

// PairState is the durable state of one pair.
type PairState struct {
	Key                 PairKey
	LastDigest          string
	LastRefCount        int
	LastSuccess         time.Time
	LastAttempt         time.Time
	LastError           string
	ConsecutiveFailures int
	// LFSDigest is the source ref digest for which LFS objects were last
	// copied to the destination ("" if never).
	LFSDigest string
	// Unarchived is set while gitmirror has unarchived the destination for
	// a sync and not yet restored its archived state (see SetUnarchived).
	Unarchived bool
}

// Run is one recorded sync run of a pair.
type Run struct {
	ID       int64
	Key      PairKey
	Trigger  string
	Started  time.Time
	Finished time.Time // zero while running
	Status   RunStatus
	Steps    []StepResult
	Error    string
}

// Store is a SQLite-backed state store, safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// SetClock replaces the store's clock; intended for tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

// Open opens (creating if needed) the database at path, creating the parent
// directory, enabling WAL, busy_timeout=5000 and foreign keys, and running
// migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("state: create dir: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := pingRetryBusy(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("state: open: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openBusyWait bounds how long Open retries a connection whose setup
// pragmas hit SQLITE_BUSY. Switching a brand new database to WAL takes an
// exclusive lock without consulting busy_timeout, so two processes opening
// it at the same moment can collide.
const openBusyWait = 5 * time.Second

// sqliteBusy is SQLite's primary result code SQLITE_BUSY.
const sqliteBusy = 5

func pingRetryBusy(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(openBusyWait)
	for {
		err := db.PingContext(ctx)
		var se *sqlite.Error
		if err == nil || !errors.As(err, &se) || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			return err
		}
		t := time.NewTimer(25 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return err
		}
	}
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

type migration struct {
	version int
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return nil, fmt.Errorf("state: bad migration name %q", name)
		}
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{v, string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	// Another process (e.g. a read-only plan next to serve) may open the
	// same fresh database at the same moment. BEGIN IMMEDIATE takes the
	// write lock up front (waiting up to busy_timeout), and the version is
	// read inside that transaction, so each migration is applied once.
	// An up-to-date database (the common case) is detected without taking
	// the write lock.
	var pre int
	if len(ms) > 0 && s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&pre) == nil &&
		pre == ms[len(ms)-1].version {
		return nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("state: migrate: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("state: migrate: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`); err != nil {
				// Never hand a connection with an open transaction back to
				// the pool: returning ErrBadConn from Raw discards it.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("state: schema_migrations: %w", err)
	}
	var cur int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return fmt.Errorf("state: read schema version: %w", err)
	}
	if len(ms) == 0 && cur > 0 || len(ms) > 0 && cur > ms[len(ms)-1].version {
		return errors.New("state: database schema is newer than this binary")
	}
	for _, m := range ms {
		if m.version <= cur {
			continue
		}
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("state: migration %04d: %w", m.version, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			m.version, s.now().UnixNano()); err != nil {
			return fmt.Errorf("state: migration %04d: %w", m.version, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("state: migrate: commit: %w", err)
	}
	committed = true
	return nil
}

func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// MarkOwned records that the tool owns the destination repo. Idempotent; a
// later call updates how. Keyed by forge and lowercase repo.
func (s *Store) MarkOwned(ctx context.Context, dstForge, dstRepo string, how Ownership) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ownership(dst_forge, dst_repo, how, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(dst_forge, dst_repo) DO UPDATE SET how = excluded.how`,
		dstForge, strings.ToLower(dstRepo), string(how), s.now().UnixNano())
	return err
}

// OwnershipRecord is a destination ownership row. RepoID and ForgeURL
// identify the repository ownership was recorded for (the forge's repository
// id and the destination forge's normalised base URL); 0 and "" mean unknown
// (rows from before identities were recorded, or a provider that reports no
// id).
type OwnershipRecord struct {
	How      Ownership
	RepoID   int64
	ForgeURL string
}

// MarkOwnedAs records that the tool owns the destination repo, together with
// its identity. Unlike MarkOwned, an existing row is overwritten entirely.
func (s *Store) MarkOwnedAs(ctx context.Context, dstForge, dstRepo string, how Ownership, repoID int64, forgeURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ownership(dst_forge, dst_repo, how, created_at, dst_repo_id, forge_url)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(dst_forge, dst_repo) DO UPDATE SET how = excluded.how, dst_repo_id = excluded.dst_repo_id,
			forge_url = excluded.forge_url`,
		dstForge, strings.ToLower(dstRepo), string(how), s.now().UnixNano(), repoID, forgeURL)
	return err
}

// OwnershipOf returns the ownership row of the destination repo; ok is false
// when it is not owned.
func (s *Store) OwnershipOf(ctx context.Context, dstForge, dstRepo string) (OwnershipRecord, bool, error) {
	var rec OwnershipRecord
	var how string
	err := s.db.QueryRowContext(ctx, `SELECT how, dst_repo_id, forge_url FROM ownership WHERE dst_forge = ? AND dst_repo = ?`,
		dstForge, strings.ToLower(dstRepo)).Scan(&how, &rec.RepoID, &rec.ForgeURL)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnershipRecord{}, false, nil
	}
	if err != nil {
		return OwnershipRecord{}, false, err
	}
	rec.How = Ownership(how)
	return rec, true, nil
}

// BackfillOwnership fills an owned destination's unknown identity fields
// (RepoID 0, ForgeURL "") with the given values; fields already set are
// never changed, and an unowned destination gets no row. Zero/empty
// arguments leave the corresponding field alone.
func (s *Store) BackfillOwnership(ctx context.Context, dstForge, dstRepo string, repoID int64, forgeURL string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ownership SET
		dst_repo_id = CASE WHEN dst_repo_id = 0 THEN ? ELSE dst_repo_id END,
		forge_url = CASE WHEN forge_url = '' THEN ? ELSE forge_url END
		WHERE dst_forge = ? AND dst_repo = ?`,
		repoID, forgeURL, dstForge, strings.ToLower(dstRepo))
	return err
}

// Owned reports whether the destination repo is owned and how.
func (s *Store) Owned(ctx context.Context, dstForge, dstRepo string) (Ownership, bool, error) {
	var how string
	err := s.db.QueryRowContext(ctx, `SELECT how FROM ownership WHERE dst_forge = ? AND dst_repo = ?`,
		dstForge, strings.ToLower(dstRepo)).Scan(&how)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return Ownership(how), true, nil
}

// ReleaseOwnership forgets ownership of the destination repo. No error if it
// was not owned.
func (s *Store) ReleaseOwnership(ctx context.Context, dstForge, dstRepo string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ownership WHERE dst_forge = ? AND dst_repo = ?`,
		dstForge, strings.ToLower(dstRepo))
	return err
}

const pairCols = `rule, src_forge, src_repo, dst_forge, dst_repo, last_digest, last_ref_count,
	last_success, last_attempt, last_error, consecutive_failures, lfs_digest, unarchived`

type scanner interface{ Scan(...any) error }

func scanPair(r scanner) (PairState, error) {
	var p PairState
	var succ, att int64
	err := r.Scan(&p.Key.Rule, &p.Key.SrcForge, &p.Key.SrcRepo, &p.Key.DstForge, &p.Key.DstRepo,
		&p.LastDigest, &p.LastRefCount, &succ, &att, &p.LastError, &p.ConsecutiveFailures, &p.LFSDigest, &p.Unarchived)
	p.LastSuccess, p.LastAttempt = fromNanos(succ), fromNanos(att)
	return p, err
}

// Pair returns the state of one pair; ok is false if it has never run.
func (s *Store) Pair(ctx context.Context, k PairKey) (PairState, bool, error) {
	p, err := scanPair(s.db.QueryRowContext(ctx, `SELECT `+pairCols+` FROM pairs WHERE key = ?`, k.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return PairState{}, false, nil
	}
	if err != nil {
		return PairState{}, false, err
	}
	return p, true, nil
}

// Pairs returns all pair states sorted by key string.
func (s *Store) Pairs(ctx context.Context) ([]PairState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pairCols+` FROM pairs ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PairState
	for rows.Next() {
		p, err := scanPair(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StartRun records a new running run for the pair (creating the pair row if
// needed), sets LastAttempt, and returns the run id.
func (s *Store) StartRun(ctx context.Context, k PairKey, trigger string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after Commit
	now := toNanos(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO pairs(key, rule, src_forge, src_repo, dst_forge, dst_repo, last_attempt)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET last_attempt = excluded.last_attempt`,
		k.String(), k.Rule, k.SrcForge, k.SrcRepo, k.DstForge, k.DstRepo, now); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO runs(pair_key, trigger, started, status) VALUES (?, ?, ?, ?)`,
		k.String(), trigger, now, string(RunRunning))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UpdateRunSteps stores the steps a run has finished so far, so its progress
// can be shown while it runs. Only a running run is changed; FinishRun later
// replaces the steps with the final list.
func (s *Store) UpdateRunSteps(ctx context.Context, id int64, steps []StepResult) error {
	if steps == nil {
		steps = []StepResult{}
	}
	sj, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET steps = ? WHERE id = ? AND status = ?`, string(sj), id, string(RunRunning))
	return err
}

// FinishRun completes a running run and updates the pair state in a single
// transaction. ok/partial: LastSuccess=now, error cleared, failures reset, and
// a non-empty newDigest updates LastDigest/LastRefCount. failed/aborted:
// LastError=errMsg, failures incremented, digest untouched. Finishing an
// unknown or already finished run is an error.
func (s *Store) FinishRun(ctx context.Context, id int64, status RunStatus, steps []StepResult, errMsg string, newDigest string, refCount int) error {
	if steps == nil {
		steps = []StepResult{}
	}
	sj, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after Commit
	now := toNanos(s.now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE runs SET finished = ?, status = ?, steps = ?, error = ?
		WHERE id = ? AND status = ?`, now, string(status), string(sj), errMsg, id, string(RunRunning))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("state: run %d not found or already finished", id)
	}
	var pk string
	if err := tx.QueryRowContext(ctx, `SELECT pair_key FROM runs WHERE id = ?`, id).Scan(&pk); err != nil {
		return err
	}
	switch status {
	case RunOK, RunPartial:
		if newDigest != "" {
			_, err = tx.ExecContext(ctx, `UPDATE pairs SET last_success = ?, last_error = '', consecutive_failures = 0,
				last_digest = ?, last_ref_count = ? WHERE key = ?`, now, newDigest, refCount, pk)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE pairs SET last_success = ?, last_error = '', consecutive_failures = 0
				WHERE key = ?`, now, pk)
		}
	case RunFailed, RunAborted:
		_, err = tx.ExecContext(ctx, `UPDATE pairs SET last_error = ?, consecutive_failures = consecutive_failures + 1
			WHERE key = ?`, errMsg, pk)
	default:
		err = fmt.Errorf("state: invalid final run status %q", status)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetLFSDigest records that LFS objects have been copied to the destination
// for the source refs with the given digest. The pair must exist (StartRun
// creates it). FinishRun leaves it unchanged.
func (s *Store) SetLFSDigest(ctx context.Context, k PairKey, digest string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE pairs SET lfs_digest = ? WHERE key = ?`, digest, k.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("state: pair %s not found", k)
	}
	return nil
}

// SetUnarchived sets or clears the pair's "unarchived by gitmirror" marker.
// The engine sets it before unarchiving a destination for a sync and clears
// it once the archived state is restored, so a run that dies in between is
// repaired by the next one. The pair must exist (StartRun creates it).
func (s *Store) SetUnarchived(ctx context.Context, k PairKey, on bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE pairs SET unarchived = ? WHERE key = ?`, on, k.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("state: pair %s not found", k)
	}
	return nil
}

func (s *Store) queryRuns(ctx context.Context, where string, limit int, args ...any) ([]Run, error) {
	q := `SELECT r.id, p.rule, p.src_forge, p.src_repo, p.dst_forge, p.dst_repo, r.trigger, r.started, r.finished,
		r.status, r.steps, r.error FROM runs r JOIN pairs p ON p.key = r.pair_key ` + where + ` ORDER BY r.id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var st, fin int64
		var status, steps string
		if err := rows.Scan(&r.ID, &r.Key.Rule, &r.Key.SrcForge, &r.Key.SrcRepo, &r.Key.DstForge, &r.Key.DstRepo,
			&r.Trigger, &st, &fin, &status, &steps, &r.Error); err != nil {
			return nil, err
		}
		r.Started, r.Finished, r.Status = fromNanos(st), fromNanos(fin), RunStatus(status)
		if err := json.Unmarshal([]byte(steps), &r.Steps); err != nil {
			return nil, fmt.Errorf("state: run %d steps: %w", r.ID, err)
		}
		if len(r.Steps) == 0 {
			r.Steps = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Runs returns runs of one pair, newest first. limit <= 0 means all.
func (s *Store) Runs(ctx context.Context, k PairKey, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE r.pair_key = ?`, limit, k.String())
}

// RecentRuns returns runs across all pairs, newest first. limit <= 0 means all.
func (s *Store) RecentRuns(ctx context.Context, limit int) ([]Run, error) {
	return s.queryRuns(ctx, ``, limit)
}

// AbandonRunning marks leftover "running" rows as aborted with error
// "interrupted" and returns how many were changed. Call at startup.
func (s *Store) AbandonRunning(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, error = 'interrupted', finished = ? WHERE status = ?`,
		string(RunAborted), s.now().UTC().UnixNano(), string(RunRunning))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PruneRuns deletes all but the keepPerPair newest runs of each pair and
// returns the number deleted.
func (s *Store) PruneRuns(ctx context.Context, keepPerPair int) (int, error) {
	if keepPerPair < 0 {
		keepPerPair = 0
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id IN (
		SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY pair_key ORDER BY id DESC) AS rn FROM runs)
		WHERE rn > ?)`, keepPerPair)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReleaseMap returns the source release id to destination release id mapping
// for a pair (never nil).
func (s *Store) ReleaseMap(ctx context.Context, k PairKey) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, dst_id FROM release_map WHERE pair_key = ?`, k.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int64{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		m[a] = b
	}
	return m, rows.Err()
}

// SetReleaseMapping upserts a source to destination release id mapping.
func (s *Store) SetReleaseMapping(ctx context.Context, k PairKey, srcID, dstID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO release_map(pair_key, src_id, dst_id) VALUES (?, ?, ?)
		ON CONFLICT(pair_key, src_id) DO UPDATE SET dst_id = excluded.dst_id`, k.String(), srcID, dstID)
	return err
}

// DeleteReleaseMapping removes the mapping for a source release id.
func (s *Store) DeleteReleaseMapping(ctx context.Context, k PairKey, srcID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM release_map WHERE pair_key = ? AND src_id = ?`, k.String(), srcID)
	return err
}

// AssetMap returns, for a pair, the destination asset id to source asset id
// mapping: which source release asset each destination asset was copied
// from (never nil).
func (s *Store) AssetMap(ctx context.Context, k PairKey) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dst_id, src_id FROM asset_map WHERE pair_key = ?`, k.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int64{}
	for rows.Next() {
		var d, src int64
		if err := rows.Scan(&d, &src); err != nil {
			return nil, err
		}
		m[d] = src
	}
	return m, rows.Err()
}

// SetAssetMapping upserts the source asset id a destination asset was
// copied from.
func (s *Store) SetAssetMapping(ctx context.Context, k PairKey, dstAssetID, srcAssetID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO asset_map(pair_key, dst_id, src_id) VALUES (?, ?, ?)
		ON CONFLICT(pair_key, dst_id) DO UPDATE SET src_id = excluded.src_id`, k.String(), dstAssetID, srcAssetID)
	return err
}

// DeleteAssetMapping removes the mapping for a destination asset id. No
// error if there is none.
func (s *Store) DeleteAssetMapping(ctx context.Context, k PairKey, dstAssetID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM asset_map WHERE pair_key = ? AND dst_id = ?`, k.String(), dstAssetID)
	return err
}

// MarkMissing records that the pair's source was reported as not found and
// returns when that was first recorded. It is idempotent: repeat calls keep
// the original first-seen time (the store clock at the first call) until
// ClearMissing removes the record.
func (s *Store) MarkMissing(ctx context.Context, k PairKey) (firstSeen time.Time, err error) {
	return s.MarkMissingObserved(ctx, k, 0)
}

// MarkMissingObserved is MarkMissing for an observation that must be
// continuous: it also records the observation time (last_seen), and when
// maxGap is positive and the previous observation is older than maxGap (or
// unknown, for a record written before last_seen existed) the missing
// period restarts: first_seen becomes now and the reported flag is reset.
// It returns the (possibly restarted) first-seen time.
func (s *Store) MarkMissingObserved(ctx context.Context, k PairKey, maxGap time.Duration) (firstSeen time.Time, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after Commit
	now := s.now().UTC()
	var first, last int64
	err = tx.QueryRowContext(ctx, `SELECT first_seen, last_seen FROM missing WHERE pair_key = ?`, k.String()).Scan(&first, &last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		first = now.UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO missing(pair_key, first_seen, last_seen) VALUES (?, ?, ?)`,
			k.String(), first, first); err != nil {
			return time.Time{}, err
		}
	case err != nil:
		return time.Time{}, err
	case maxGap > 0 && (last == 0 || now.Sub(fromNanos(last)) > maxGap):
		first = now.UnixNano()
		if _, err := tx.ExecContext(ctx, `UPDATE missing SET first_seen = ?, last_seen = ?, reported = 0 WHERE pair_key = ?`,
			first, first, k.String()); err != nil {
			return time.Time{}, err
		}
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE missing SET last_seen = ? WHERE pair_key = ?`,
			now.UnixNano(), k.String()); err != nil {
			return time.Time{}, err
		}
	}
	return fromNanos(first), tx.Commit()
}

// MissingRecord returns the pair's missing record: when the source was
// first and last recorded as not found (last is zero for a record written
// before last_seen existed); ok is false if there is no record.
func (s *Store) MissingRecord(ctx context.Context, k PairKey) (first, last time.Time, ok bool, err error) {
	var f, l int64
	err = s.db.QueryRowContext(ctx, `SELECT first_seen, last_seen FROM missing WHERE pair_key = ?`, k.String()).Scan(&f, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return fromNanos(f), fromNanos(l), true, nil
}

// ClearMissing forgets that the pair's source was missing, restarting its
// deletion grace period. No error if there is no record.
func (s *Store) ClearMissing(ctx context.Context, k PairKey) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM missing WHERE pair_key = ?`, k.String())
	return err
}

// MissingSince returns when the pair's source was first recorded as not
// found; ok is false if there is no such record.
func (s *Store) MissingSince(ctx context.Context, k PairKey) (time.Time, bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT first_seen FROM missing WHERE pair_key = ?`, k.String()).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return fromNanos(n), true, nil
}

// DeletePair forgets a pair entirely in one transaction: its pair row, run
// history, release and asset id mappings and missing record. Destination ownership is
// not touched (see ReleaseOwnership), and neither is the audit log, which
// keeps the pair's events. No error if the pair is unknown.
func (s *Store) DeletePair(ctx context.Context, k PairKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after Commit
	ks := k.String()
	for _, q := range []string{
		`DELETE FROM runs WHERE pair_key = ?`,
		`DELETE FROM release_map WHERE pair_key = ?`,
		`DELETE FROM asset_map WHERE pair_key = ?`,
		`DELETE FROM missing WHERE pair_key = ?`,
		`DELETE FROM pairs WHERE key = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, ks); err != nil {
			return err
		}
	}
	return tx.Commit()
}
