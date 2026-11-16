package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UISession is a web UI sign-in session. IDHash is the SHA-256 of the
// session token held by the browser; the token itself is never stored.
// Times are set by the caller (the web UI's clock).
type UISession struct {
	IDHash   []byte
	CSRF     string
	Created  time.Time
	LastSeen time.Time
}

// CreateUISession stores a new session.
func (s *Store) CreateUISession(ctx context.Context, x UISession) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ui_sessions(id_hash, csrf, created_at, last_seen) VALUES (?, ?, ?, ?)`,
		x.IDHash, x.CSRF, toNanos(x.Created), toNanos(x.LastSeen)); err != nil {
		return fmt.Errorf("state: create ui session: %w", err)
	}
	return nil
}

// UISession returns the session with the given token hash; ok is false if
// there is none.
func (s *Store) UISession(ctx context.Context, idHash []byte) (x UISession, ok bool, err error) {
	var c, l int64
	err = s.db.QueryRowContext(ctx, `SELECT id_hash, csrf, created_at, last_seen FROM ui_sessions WHERE id_hash = ?`, idHash).
		Scan(&x.IDHash, &x.CSRF, &c, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return UISession{}, false, nil
	}
	if err != nil {
		return UISession{}, false, fmt.Errorf("state: read ui session: %w", err)
	}
	x.Created, x.LastSeen = fromNanos(c), fromNanos(l)
	return x, true, nil
}

// TouchUISession sets the session's last-seen time. No error if it does not
// exist.
func (s *Store) TouchUISession(ctx context.Context, idHash []byte, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE ui_sessions SET last_seen = ? WHERE id_hash = ?`, toNanos(at), idHash); err != nil {
		return fmt.Errorf("state: touch ui session: %w", err)
	}
	return nil
}

// DeleteUISession removes one session. No error if it does not exist.
func (s *Store) DeleteUISession(ctx context.Context, idHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("state: delete ui session: %w", err)
	}
	return nil
}

// DeleteOtherUISessions removes every session except keep; a nil keep
// removes them all.
func (s *Store) DeleteOtherUISessions(ctx context.Context, keep []byte) error {
	var err error
	if keep == nil {
		_, err = s.db.ExecContext(ctx, `DELETE FROM ui_sessions`)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE id_hash <> ?`, keep)
	}
	if err != nil {
		return fmt.Errorf("state: delete ui sessions: %w", err)
	}
	return nil
}

// DeleteExpiredUISessions removes sessions last seen before idleBefore or
// created before createdBefore, and returns how many were removed.
func (s *Store) DeleteExpiredUISessions(ctx context.Context, idleBefore, createdBefore time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE last_seen < ? OR created_at < ?`,
		toNanos(idleBefore), toNanos(createdBefore))
	if err != nil {
		return 0, fmt.Errorf("state: delete expired ui sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("state: delete expired ui sessions: %w", err)
	}
	return n, nil
}
