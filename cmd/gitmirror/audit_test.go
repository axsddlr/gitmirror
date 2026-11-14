package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusAudit(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.addSource("alice", "b")
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 0, out, errOut)

	// Without --audit no audit section is printed.
	code, out, errOut = e.cli("status")
	wantCode(t, code, 0, out, errOut)
	if strings.Contains(out, "audit") {
		t.Fatalf("audit printed without --audit:\n%s", out)
	}

	code, out, errOut = e.cli("status", "--audit", "1")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "audit log (1)") || !strings.Contains(out, "create") || strings.Count(out, "mirrors/") != 3 {
		// two pair rows plus exactly one audit row
		t.Fatalf("status --audit 1:\n%s", out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatalf("token printed:\n%s", out)
	}

	code, out, errOut = e.cli("status", "--json", "--audit", "5")
	wantCode(t, code, 0, out, errOut)
	var doc struct {
		Audit []struct {
			ID          int64  `json:"id"`
			Action      string `json:"action"`
			Rule        string `json:"rule"`
			Destination string `json:"destination"`
		} `json:"audit"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Audit) != 2 {
		t.Fatalf("doc = %+v err %v\n%s", doc, err, out)
	}
	if doc.Audit[0].ID <= doc.Audit[1].ID || doc.Audit[0].Action != "create" || doc.Audit[0].Rule != "r1" ||
		!strings.HasPrefix(doc.Audit[0].Destination, "dst:mirrors/") {
		t.Fatalf("audit = %+v", doc.Audit)
	}
	evs, err := e.openState().AuditLog(context.Background(), 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("AuditLog = %+v, %v", evs, err)
	}
}
