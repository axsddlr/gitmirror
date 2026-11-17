package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"gitmirror/internal/state"
)

// ResetAdmin removes the admin password and every web UI session from st,
// so the next UI start shows the first-run setup page with a new setup code.
// Forges, rules and other settings are kept. The caller must hold the state
// dir lock (no serve running), since a running UI caches that an admin
// exists.
func ResetAdmin(ctx context.Context, st *state.Store) error {
	if err := st.DeleteSetting(ctx, adminPasswordKey); err != nil {
		return err
	}
	return st.DeleteOtherUISessions(ctx, nil)
}

// resetAdminTokenKey stores the last GITMIRROR_RESET_ADMIN value that was
// acted on, so a value left in place does not reset again on every start.
const resetAdminTokenKey = "admin_reset_token"

// ResetAdminOnce runs ResetAdmin when token is a value that has not been
// used before, and records it. It reports whether it reset. An empty token,
// "0" and "false" (any case) mean off. Like ResetAdmin it must run before
// the UI starts, with the state dir lock held.
func ResetAdminOnce(ctx context.Context, st *state.Store, token string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" || token == "0" || strings.EqualFold(token, "false") {
		return false, nil
	}
	sum := sha256.Sum256([]byte(token))
	if prev, ok, err := st.Setting(ctx, resetAdminTokenKey); err != nil {
		return false, err
	} else if ok && subtle.ConstantTimeCompare(prev, sum[:]) == 1 {
		return false, nil
	}
	if err := ResetAdmin(ctx, st); err != nil {
		return false, err
	}
	return true, st.PutSetting(ctx, resetAdminTokenKey, sum[:])
}
