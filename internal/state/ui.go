package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UIForge is a forge managed in the web UI. SealedToken is the token as
// sealed by the app layer; the store never handles plaintext tokens.
type UIForge struct {
	Name, Type, URL, Username string
	SealedToken               []byte
	// Created and Updated are set by the store.
	Created, Updated time.Time
}

// UIRule is a mirror rule managed in the web UI, stored as the JSON of a
// config.Rule.
type UIRule struct {
	Name string
	JSON []byte
	// Created and Updated are set by the store.
	Created, Updated time.Time
}

// UIChanges is a set of UI forge/rule writes applied in one transaction by
// ApplyUI. Deletes run before puts; deleting an unknown name is not an error.
type UIChanges struct {
	PutForges    []UIForge
	DeleteForges []string
	PutRules     []UIRule
	DeleteRules  []string
}

// Setting is one key/value row of the settings table.
type Setting struct {
	Key   string
	Value []byte
}

// UIForges returns the UI-managed forges sorted by name.
func (s *Store) UIForges(ctx context.Context) ([]UIForge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, url, username, token, created_at, updated_at
		FROM ui_forges ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("state: list ui forges: %w", err)
	}
	defer rows.Close()
	var out []UIForge
	for rows.Next() {
		var f UIForge
		var c, u int64
		if err := rows.Scan(&f.Name, &f.Type, &f.URL, &f.Username, &f.SealedToken, &c, &u); err != nil {
			return nil, fmt.Errorf("state: list ui forges: %w", err)
		}
		f.Created, f.Updated = fromNanos(c), fromNanos(u)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list ui forges: %w", err)
	}
	return out, nil
}

// UIRules returns the UI-managed rules sorted by name.
func (s *Store) UIRules(ctx context.Context) ([]UIRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, rule, created_at, updated_at FROM ui_rules ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("state: list ui rules: %w", err)
	}
	defer rows.Close()
	var out []UIRule
	for rows.Next() {
		var r UIRule
		var js string
		var c, u int64
		if err := rows.Scan(&r.Name, &js, &c, &u); err != nil {
			return nil, fmt.Errorf("state: list ui rules: %w", err)
		}
		r.JSON, r.Created, r.Updated = []byte(js), fromNanos(c), fromNanos(u)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list ui rules: %w", err)
	}
	return out, nil
}

// ApplyUI applies ch in a single transaction: either every write happens or
// none. A put of an existing name replaces it and keeps its creation time.
func (s *Store) ApplyUI(ctx context.Context, ch UIChanges) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: apply ui changes: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after Commit
	now := s.now().UnixNano()
	for _, n := range ch.DeleteForges {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_forges WHERE name = ?`, n); err != nil {
			return fmt.Errorf("state: delete ui forge %q: %w", n, err)
		}
	}
	for _, n := range ch.DeleteRules {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_rules WHERE name = ?`, n); err != nil {
			return fmt.Errorf("state: delete ui rule %q: %w", n, err)
		}
	}
	for _, f := range ch.PutForges {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_forges(name, type, url, username, token, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET type = excluded.type, url = excluded.url, username = excluded.username,
				token = excluded.token, updated_at = excluded.updated_at`,
			f.Name, f.Type, f.URL, f.Username, f.SealedToken, now, now); err != nil {
			return fmt.Errorf("state: put ui forge %q: %w", f.Name, err)
		}
	}
	for _, r := range ch.PutRules {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_rules(name, rule, created_at, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET rule = excluded.rule, updated_at = excluded.updated_at`,
			r.Name, string(r.JSON), now, now); err != nil {
			return fmt.Errorf("state: put ui rule %q: %w", r.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: apply ui changes: %w", err)
	}
	return nil
}

// Setting returns the value of key; ok is false if it is not set.
func (s *Store) Setting(ctx context.Context, key string) (value []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state: read setting %q: %w", key, err)
	}
	return value, true, nil
}

// Settings returns every setting sorted by key.
func (s *Store) Settings(ctx context.Context) ([]Setting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("state: list settings: %w", err)
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var st Setting
		if err := rows.Scan(&st.Key, &st.Value); err != nil {
			return nil, fmt.Errorf("state: list settings: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list settings: %w", err)
	}
	return out, nil
}

// PutSetting sets key to value, replacing any previous value.
func (s *Store) PutSetting(ctx context.Context, key string, value []byte) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("state: put setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes key. No error if it is not set.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("state: delete setting %q: %w", key, err)
	}
	return nil
}
