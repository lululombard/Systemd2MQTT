package app

import (
	"context"
	"errors"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
)

// scheduleReconcile debounces wake hints: the first one arms a timer, later
// ones within the window are folded into it. The timer only enqueues.
func (a *App) scheduleReconcile(scope config.Scope) {
	if a.pending[scope] {
		return
	}
	a.pending[scope] = true
	time.AfterFunc(reconcileDelay, func() { a.enqueue(evReconcile{scope: scope}) })
}

// reconcileScope queries one scope and publishes whatever moved. Signals are
// hints, this is the truth. It runs on the loop goroutine; Query is a single
// fast D-Bus call bounded by queryTimeout, and Supervisor.Query also tracks
// liveness for us (a dead bus gets redialed after this fails).
func (a *App) reconcileScope(scope config.Scope) {
	// Clear the debounce flag first so a dropped timer event can never
	// wedge the scope; the poll ticker lands here too.
	a.pending[scope] = false
	sup, ok := a.sup[scope]
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, queryTimeout)
	defer cancel()
	statuses, err := sup.Query(ctx)
	if err != nil {
		if errors.Is(err, systemd.ErrOffline) {
			a.log.Debug("reconcile skipped, bus offline", "scope", string(scope))
		} else {
			a.log.Warn("query failed", "scope", string(scope), "err", err)
		}
		return
	}
	now := time.Now()
	for _, st := range statuses {
		snap, changed, ok := a.st.ApplyUnit(scope, st, now)
		if !ok {
			a.log.Debug("query returned an unknown unit", "scope", string(scope), "unit", st.Name)
			continue
		}
		if changed {
			a.log.Info("unit state", "unit", snap.Name, "scope", string(scope),
				"active_state", snap.ActiveState, "sub_state", snap.SubState, "load_state", snap.LoadState)
		}
		a.publishUnit(snap)
	}
	for _, sel := range a.cfg.Selects() {
		if sel.Scope == scope {
			a.publishTemplate(sel)
		}
	}
}

// handleJobDone records a job outcome, refreshes the attributes and schedules
// the query that decides what the switch shows.
func (a *App) handleJobDone(e evJobDone) {
	u, ok := a.cfg.UnitByID(e.id)
	if !ok {
		return
	}
	if e.err != nil {
		a.log.Warn("job failed", "unit", u.Name, "verb", e.verb, "result", string(e.result), "err", e.err)
	} else if e.result != systemd.JobDone {
		a.log.Warn("job did not complete", "unit", u.Name, "verb", e.verb, "result", string(e.result))
	} else {
		a.log.Info("job done", "unit", u.Name, "verb", e.verb)
	}
	snap, ok := a.st.SetJobResult(e.id, e.verb, e.result, e.err, time.Now())
	if ok && snap.Known {
		a.publishUnit(snap)
	}
	a.scheduleReconcile(u.Scope)
}
