package web

import (
	"context"

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
