package engine

import (
	"context"
	"log/slog"

	"gitmirror/internal/state"
)

// audit appends an event to the store's audit log. It is called only after
// the action really happened (never in a dry run). The action has already
// taken effect, so a failure to record it is logged rather than failing the
// run. detail must never contain a secret (no error text from forge calls).
func (e *Engine) audit(ctx context.Context, log *slog.Logger, k state.PairKey, action state.AuditAction, detail string) {
	if err := e.Store.Audit(context.WithoutCancel(ctx), state.Event{Key: k, Action: action, Detail: detail}); err != nil {
		log.Error("engine: record audit event", "action", action, "err", err)
	}
}
