package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AuditAction is the kind of destination action recorded in the audit log.
type AuditAction string

// AuditAction values.
const (
	// AuditCreate means gitmirror created the destination repository.
	AuditCreate AuditAction = "create"
	// AuditAdopt means gitmirror adopted an existing destination repository.
	AuditAdopt AuditAction = "adopt"
	// AuditArchive means gitmirror archived the destination repository.
	AuditArchive AuditAction = "archive"
	// AuditDelete means gitmirror deleted the destination repository.
	AuditDelete AuditAction = "delete"
)

func (a AuditAction) valid() bool {
	switch a {
	case AuditCreate, AuditAdopt, AuditArchive, AuditDelete:
		return true
	}
	return false
}

// Event is one entry of the append-only audit log. The log is never pruned
// and DeletePair leaves it alone, so it outlives the pair and its runs.
type Event struct {
	// ID increases with every event.
	ID int64
	// Time is when the event was recorded (UTC). Audit fills it from the
	// store clock when zero.
	Time time.Time
	// Key is the pair the action was taken for.
	Key PairKey
	// Action is what was done to the destination.
	Action AuditAction
	// Detail is a short human-readable note; it must never carry secrets.
	Detail string
}

// Audit appends ev to the audit log.
func (s *Store) Audit(ctx context.Context, ev Event) error {
	if !ev.Action.valid() {
		return fmt.Errorf("state: unknown audit action %q", ev.Action)
	}
	at := ev.Time
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit(time, pair_key, rule, action, dst_forge, dst_repo, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, toNanos(at), ev.Key.String(), ev.Key.Rule, string(ev.Action),
		ev.Key.DstForge, ev.Key.DstRepo, ev.Detail)
	return err
}

// AuditLog returns audit events, newest first. limit <= 0 means all.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]Event, error) {
	q := `SELECT id, time, pair_key, action, detail FROM audit ORDER BY id DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var at int64
		var key, action string
		if err := rows.Scan(&ev.ID, &at, &key, &action, &ev.Detail); err != nil {
			return nil, err
		}
		var parts []string
		if err := json.Unmarshal([]byte(key), &parts); err != nil || len(parts) != 5 {
			return nil, fmt.Errorf("state: audit event %d: bad pair key %q", ev.ID, key)
		}
		ev.Key = PairKey{Rule: parts[0], SrcForge: parts[1], SrcRepo: parts[2], DstForge: parts[3], DstRepo: parts[4]}
		ev.Time, ev.Action = fromNanos(at), AuditAction(action)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MissingReported reports whether the current missing record of the pair is
// flagged as reported (SetMissingReported). False when there is no record.
func (s *Store) MissingReported(ctx context.Context, k PairKey) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT reported FROM missing WHERE pair_key = ?`, k.String()).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// SetMissingReported flags the pair's missing record as reported, so a
// reconcile outcome that changes nothing is recorded only once per missing
// period. ClearMissing removes the record and with it the flag. No error
// (and no effect) if there is no record.
func (s *Store) SetMissingReported(ctx context.Context, k PairKey) error {
	_, err := s.db.ExecContext(ctx, `UPDATE missing SET reported = 1 WHERE pair_key = ?`, k.String())
	return err
}
