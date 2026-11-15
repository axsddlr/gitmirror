package state

import (
	"bytes"
	"testing"
	"time"
)

func TestSessionsRoundTrip(t *testing.T) {
	s := openTemp(t)
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	a := UISession{IDHash: []byte("hash-a"), CSRF: "csrf-a", Created: t0, LastSeen: t0}
	b := UISession{IDHash: []byte("hash-b"), CSRF: "csrf-b", Created: t0, LastSeen: t0}
	for _, x := range []UISession{a, b} {
		if err := s.CreateUISession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.UISession(ctx, []byte("hash-a"))
	if err != nil || !ok {
		t.Fatalf("UISession = %v, %v", ok, err)
	}
	if !bytes.Equal(got.IDHash, a.IDHash) || got.CSRF != "csrf-a" || !got.Created.Equal(t0) || !got.LastSeen.Equal(t0) {
		t.Fatalf("got %+v", got)
	}
	if _, ok, err := s.UISession(ctx, []byte("nope")); err != nil || ok {
		t.Fatalf("unknown session = %v, %v", ok, err)
	}

	t1 := t0.Add(time.Hour)
	if err := s.TouchUISession(ctx, []byte("hash-a"), t1); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.UISession(ctx, []byte("hash-a")); !got.LastSeen.Equal(t1) || !got.Created.Equal(t0) {
		t.Fatalf("after touch %+v", got)
	}

	if err := s.DeleteUISession(ctx, []byte("hash-a")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UISession(ctx, []byte("hash-a")); ok {
		t.Fatal("session survived delete")
	}
	if _, ok, _ := s.UISession(ctx, []byte("hash-b")); !ok {
		t.Fatal("other session deleted")
	}
}

func TestDeleteOtherUISessions(t *testing.T) {
	s := openTemp(t)
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, h := range []string{"a", "b", "c"} {
		if err := s.CreateUISession(ctx, UISession{IDHash: []byte(h), CSRF: h, Created: t0, LastSeen: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteOtherUISessions(ctx, []byte("b")); err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]bool{"a": false, "b": true, "c": false} {
		if _, ok, _ := s.UISession(ctx, []byte(h)); ok != want {
			t.Errorf("%s present = %v, want %v", h, ok, want)
		}
	}
	if err := s.DeleteOtherUISessions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UISession(ctx, []byte("b")); ok {
		t.Fatal("nil keep should delete every session")
	}
}

func TestDeleteExpiredUISessions(t *testing.T) {
	s := openTemp(t)
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	rows := []UISession{
		{IDHash: []byte("fresh"), Created: t0, LastSeen: t0.Add(10 * time.Hour)},
		{IDHash: []byte("idle"), Created: t0, LastSeen: t0.Add(time.Hour)},
		{IDHash: []byte("old"), Created: t0.Add(-8 * 24 * time.Hour), LastSeen: t0.Add(10 * time.Hour)},
	}
	for _, r := range rows {
		r.CSRF = "x"
		if err := s.CreateUISession(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// idle: last seen before t0+2h; absolute: created before t0-7d.
	n, err := s.DeleteExpiredUISessions(ctx, t0.Add(2*time.Hour), t0.Add(-7*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v; want 2", n, err)
	}
	if _, ok, _ := s.UISession(ctx, []byte("fresh")); !ok {
		t.Fatal("fresh session deleted")
	}
}

func TestSessionsSurviveReopen(t *testing.T) {
	p := t.TempDir() + "/state.db"
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if err := s.CreateUISession(ctx, UISession{IDHash: []byte("h"), CSRF: "c", Created: t0, LastSeen: t0}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.UISession(ctx, []byte("h")); err != nil || !ok {
		t.Fatalf("after reopen: %v %v", ok, err)
	}
	if v := schemaVersion(t, s); v < 8 {
		t.Fatalf("schema version = %d", v)
	}
}
