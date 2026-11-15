package state

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUIForgesRoundTrip(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	if got, err := s.UIForges(ctx); err != nil || len(got) != 0 {
		t.Fatalf("UIForges on a fresh store = %v, %v", got, err)
	}
	b := UIForge{Name: "b", Type: "gitea", URL: "https://g.example", Username: "u", SealedToken: []byte{1, 2, 3}}
	a := UIForge{Name: "a", Type: "github", SealedToken: []byte{4}}
	if err := s.ApplyUI(ctx, UIChanges{PutForges: []UIForge{b, a}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UIForges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("UIForges = %+v, want a, b", got)
	}
	g := got[1]
	if g.Type != "gitea" || g.URL != "https://g.example" || g.Username != "u" || !reflect.DeepEqual(g.SealedToken, []byte{1, 2, 3}) {
		t.Fatalf("forge b = %+v", g)
	}
	if g.Created.IsZero() || !g.Created.Equal(g.Updated) {
		t.Fatalf("created/updated = %v / %v", g.Created, g.Updated)
	}

	// Updating keeps created and moves updated.
	b.URL = "https://h.example"
	if err := s.ApplyUI(ctx, UIChanges{PutForges: []UIForge{b}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UIForges(ctx)
	if got[1].URL != "https://h.example" || !got[1].Created.Equal(g.Created) || !got[1].Updated.After(g.Updated) {
		t.Fatalf("updated forge b = %+v (before %+v)", got[1], g)
	}

	if err := s.ApplyUI(ctx, UIChanges{DeleteForges: []string{"a", "ghost"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UIForges(ctx)
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("after delete = %+v", got)
	}
}

func TestUIRulesRoundTrip(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	r2 := UIRule{Name: "r2", JSON: []byte(`{"name":"r2"}`)}
	r1 := UIRule{Name: "r1", JSON: []byte(`{"name":"r1"}`)}
	if err := s.ApplyUI(ctx, UIChanges{PutRules: []UIRule{r2, r1}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UIRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "r1" || string(got[1].JSON) != `{"name":"r2"}` || got[0].Created.IsZero() {
		t.Fatalf("UIRules = %+v", got)
	}
	if err := s.ApplyUI(ctx, UIChanges{DeleteRules: []string{"r1"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UIRules(ctx)
	if len(got) != 1 || got[0].Name != "r2" {
		t.Fatalf("after delete = %+v", got)
	}
}

func TestApplyUIIsAtomic(t *testing.T) {
	s := openTemp(t)
	if err := s.ApplyUI(ctx, UIChanges{PutForges: []UIForge{{Name: "a", Type: "github", SealedToken: []byte{1}}}}); err != nil {
		t.Fatal(err)
	}
	// An empty name violates the schema: the whole change must roll back.
	err := s.ApplyUI(ctx, UIChanges{
		DeleteForges: []string{"a"},
		PutRules:     []UIRule{{Name: "ok", JSON: []byte(`{}`)}},
		PutForges:    []UIForge{{Name: "", Type: "github"}},
	})
	if err == nil {
		t.Fatal("invalid change accepted")
	}
	fs, _ := s.UIForges(ctx)
	rs, _ := s.UIRules(ctx)
	if len(fs) != 1 || len(rs) != 0 {
		t.Fatalf("partial change persisted: forges %+v rules %+v", fs, rs)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, ok, err := s.Setting(ctx, "webhook_secret"); err != nil || ok {
		t.Fatalf("Setting on a fresh store = %v, %v", ok, err)
	}
	if err := s.PutSetting(ctx, "webhook_secret", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "external_url", []byte("https://x")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "webhook_secret", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Setting(ctx, "webhook_secret")
	if err != nil || !ok || string(v) != "v2" {
		t.Fatalf("Setting = %q, %v, %v", v, ok, err)
	}
	all, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Key != "external_url" || string(all[1].Value) != "v2" {
		t.Fatalf("Settings = %+v", all)
	}
	if err := s.DeleteSetting(ctx, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSetting(ctx, "never-set"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Setting(ctx, "webhook_secret"); ok {
		t.Fatal("setting survived delete")
	}
}

func TestMigration0007FromV6(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v6.db")
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
		if m.version > 6 {
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
	if v := schemaVersion(t, s); v < 7 {
		t.Fatalf("schema version = %d", v)
	}
	if _, ok, err := s.OwnershipOf(ctx, "gt", "o/a"); err != nil || !ok {
		t.Fatalf("ownership lost: %v %v", ok, err)
	}
	if err := s.ApplyUI(ctx, UIChanges{PutRules: []UIRule{{Name: "r", JSON: []byte(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
}
