package app

import (
	"context"
	"errors"
	"strings"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/ha"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
)

const polkitHint = "run systemd2mqtt --print-polkit-rule and install the output as /etc/polkit-1/rules.d/10-systemd2mqtt.rules"

// parseCommand maps a switch or button payload to a systemd verb.
func parseCommand(payload []byte) (string, bool) {
	switch strings.TrimSpace(string(payload)) {
	case "ON", "on", "1", "true":
		return "start", true
	case "OFF", "off", "0", "false":
		return "stop", true
	case "RESTART", "restart":
		return "restart", true
	}
	return "", false
}

// handleInbound routes one subscribed message. Retained messages on command
// topics are dropped so a restart never replays a stale command; the retained
// HA birth is the one retained message we act on.
func (a *App) handleInbound(e evInbound) {
	in, ok := a.t.Parse(e.topic)
	if !ok {
		a.log.Debug("message on an unexpected topic", "topic", e.topic)
		return
	}
	if e.retained && in.Kind != ha.InHAStatus {
		a.log.Debug("ignoring retained command", "topic", e.topic, "payload", string(e.payload))
		return
	}
	payload := strings.TrimSpace(string(e.payload))

	switch in.Kind {
	case ha.InHAStatus:
		if payload == "online" {
			a.log.Info("home assistant is online, scheduling a full publish")
			a.requestFullPublish()
		} else {
			a.log.Info("home assistant status", "status", payload)
		}

	case ha.InUnitSet:
		u, ok := a.cfg.UnitByID(in.ID)
		if !ok {
			a.log.Warn("command for an unknown unit", "id", in.ID)
			return
		}
		verb, ok := parseCommand(e.payload)
		if !ok {
			a.log.Warn("unknown unit command", "unit", u.Name, "payload", payload)
			return
		}
		a.log.Info("unit command", "unit", u.Name, "verb", verb)
		a.dispatchUnit(u, verb)

	case ha.InTemplateSet:
		sel, ok := a.selectByID(in.ID)
		if !ok {
			a.log.Warn("command for an unknown template", "id", in.ID)
			return
		}
		a.dispatchTemplate(sel, payload)

	case ha.InDisplaySet:
		switch strings.ToUpper(payload) {
		case "ON", "1", "TRUE":
			a.displayCommand(display.ModeOn)
		case "OFF", "0", "FALSE":
			a.displayCommand(display.Mode(a.cfg.Display.OffModeValue()))
		default:
			a.log.Warn("unknown display command", "payload", payload)
		}

	case ha.InDisplayModeSet:
		m, err := display.ParseMode(payload)
		if err != nil {
			a.log.Warn("unknown display mode", "payload", payload)
			return
		}
		a.displayCommand(m)

	case ha.InDaemonRestart:
		a.log.Info("restart requested over mqtt, exiting so systemd restarts us")
		a.cancel()
	}
}

// Unit workers: one goroutine per unit with a cap-1 mailbox. A command that
// arrives while one is still running replaces the pending one, so a burst of
// clicks ends with the last click winning and never queues up.

type unitWorker struct {
	unit    config.UnitConfig
	mailbox chan string
}

func (a *App) dispatchUnit(u config.UnitConfig, verb string) {
	id := u.ObjectID()
	w, ok := a.workers[id]
	if !ok {
		w = &unitWorker{unit: u, mailbox: make(chan string, 1)}
		a.workers[id] = w
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			w.run(a.ctx, a)
		}()
	}
	if replaced := offerReplace(w.mailbox, verb); replaced {
		a.log.Info("replaced pending command", "unit", u.Name, "verb", verb)
	}
}

// offerReplace puts v in a cap-1 mailbox, dropping whatever was waiting.
func offerReplace[T any](mailbox chan T, v T) (replaced bool) {
	select {
	case mailbox <- v:
		return false
	default:
	}
	select {
	case <-mailbox:
		replaced = true
	default:
	}
	select {
	case mailbox <- v:
	default:
	}
	return replaced
}

func (w *unitWorker) run(ctx context.Context, a *App) {
	for {
		select {
		case verb := <-w.mailbox:
			res, err := a.runJob(ctx, w.unit, verb)
			a.enqueue(evJobDone{id: w.unit.ObjectID(), verb: verb, result: res, err: err})
		case <-ctx.Done():
			return
		}
	}
}

// runJob runs one verb against the scope's current manager. It is called from
// worker goroutines only, never from the loop.
func (a *App) runJob(ctx context.Context, u config.UnitConfig, verb string) (systemd.JobResult, error) {
	sup, ok := a.sup[u.Scope]
	if !ok {
		return systemd.JobError, systemd.ErrOffline
	}
	m, online := sup.Manager()
	if !online || m == nil {
		return systemd.JobError, systemd.ErrOffline
	}
	jctx, cancel := context.WithTimeout(ctx, a.cfg.Systemd.JobTimeout)
	defer cancel()
	mode := a.cfg.Systemd.JobMode

	var (
		res systemd.JobResult
		err error
	)
	switch verb {
	case "start":
		res, err = m.Start(jctx, u.Name, mode)
	case "stop":
		res, err = m.Stop(jctx, u.Name, mode)
	case "restart":
		res, err = m.Restart(jctx, u.Name, mode)
	default:
		return systemd.JobError, errors.New("unknown verb " + verb)
	}
	if systemd.IsPolkitDenied(err) {
		a.log.Warn("polkit denied the job", "unit", u.Name, "verb", verb, "scope", string(u.Scope), "hint", polkitHint)
	}
	return res, err
}

// Template workers sequence an exclusive select: stop every other instance,
// wait for each result, then start the chosen one. Same cap-1 newest-wins
// mailbox as units, so picking twice fast only runs the last pick.

type templateCmd struct {
	label string
	stop  []config.UnitConfig
	start *config.UnitConfig
}

type templateWorker struct {
	sel     config.TemplateSelect
	mailbox chan templateCmd
}

func (a *App) dispatchTemplate(sel config.TemplateSelect, label string) {
	cmd := templateCmd{label: label}
	var chosen *config.SelectOption
	if label != "off" {
		for i := range sel.Options {
			if sel.Options[i].Label == label {
				chosen = &sel.Options[i]
				break
			}
		}
		if chosen == nil {
			a.log.Warn("unknown template option", "template", sel.ID, "label", label)
			return
		}
	}
	for _, opt := range sel.Options {
		u, ok := a.cfg.UnitByID(opt.UnitID)
		if !ok {
			continue
		}
		if chosen != nil && opt.UnitID == chosen.UnitID {
			uc := u
			cmd.start = &uc
			continue
		}
		// A unit systemd cannot find cannot be running, and stopping it would only error.
		if snap, known := a.st.Unit(opt.UnitID); known && snap.Known && snap.LoadState == "not-found" {
			continue
		}
		cmd.stop = append(cmd.stop, u)
	}
	a.log.Info("template command", "template", sel.ID, "label", label, "stop", len(cmd.stop), "start", cmd.start != nil)

	w, ok := a.tmplWorkers[sel.ID]
	if !ok {
		w = &templateWorker{sel: sel, mailbox: make(chan templateCmd, 1)}
		a.tmplWorkers[sel.ID] = w
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			w.run(a.ctx, a)
		}()
	}
	if offerReplace(w.mailbox, cmd) {
		a.log.Info("replaced pending template command", "template", sel.ID, "label", label)
	}
}

func (w *templateWorker) run(ctx context.Context, a *App) {
	for {
		select {
		case cmd := <-w.mailbox:
			a.enqueue(evTemplateDone{id: w.sel.ID, label: cmd.label, err: a.runTemplate(ctx, cmd)})
		case <-ctx.Done():
			return
		}
	}
}

func (a *App) runTemplate(ctx context.Context, cmd templateCmd) error {
	var failed error
	for _, u := range cmd.stop {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := a.runJob(ctx, u, "stop")
		a.enqueue(evJobDone{id: u.ObjectID(), verb: "stop", result: res, err: err})
		if err != nil {
			failed = errors.Join(failed, errors.New("stop "+u.Name+": "+err.Error()))
		} else if res != systemd.JobDone {
			failed = errors.Join(failed, errors.New("stop "+u.Name+": "+string(res)))
		}
	}
	if cmd.start == nil {
		return failed
	}
	if failed != nil {
		// Starting anyway would break the one-instance promise the select makes.
		return errors.Join(failed, errors.New("not starting "+cmd.start.Name))
	}
	res, err := a.runJob(ctx, *cmd.start, "start")
	a.enqueue(evJobDone{id: cmd.start.ObjectID(), verb: "start", result: res, err: err})
	if err != nil {
		return errors.New("start " + cmd.start.Name + ": " + err.Error())
	}
	if res != systemd.JobDone {
		return errors.New("start " + cmd.start.Name + ": " + string(res))
	}
	return nil
}

// displayCommand writes a mode from a helper goroutine and re-reads it; the
// loop learns the outcome through evDisplayMode.
func (a *App) displayCommand(m display.Mode) {
	d := a.display()
	if d == nil {
		a.log.Warn("display command ignored, display not connected", "mode", m.String())
		return
	}
	a.log.Info("display command", "mode", m.String())
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, displayCallTimeout)
		defer cancel()
		if err := d.Set(ctx, m); err != nil {
			if errors.Is(err, display.ErrUnknownState) {
				a.log.Warn("display command refused, power state unknown", "mode", m.String())
			} else {
				a.log.Warn("display set failed", "mode", m.String(), "err", err)
			}
		}
		cur, err := d.Get(ctx)
		if err != nil {
			a.enqueue(evDisplayError{err: err})
			return
		}
		a.enqueue(evDisplayMode{mode: cur})
	}()
}
