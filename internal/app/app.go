// Package app wires systemd, the display and MQTT together behind one event loop.
//
// The loop in Run is the only goroutine that touches State, the publish cache
// and the MQTT publisher. Supervisors, the display runner, paho callbacks, job
// workers and timers only enqueue events; D-Bus calls that can block run in
// workers and come back as events too. The one exception is Supervisor.Query,
// which the loop calls directly under a short timeout because it is the truth
// source for every state we publish.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/ha"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
	"github.com/lululombard/Systemd2MQTT/internal/version"
)

// Tunables, package-level so tests can shrink them.
var (
	// fullPublishDelay coalesces MQTT connect, the retained HA birth and HA
	// restart flaps into one republish.
	fullPublishDelay = 2 * time.Second
	// reconcileDelay debounces wake hints before the query.
	reconcileDelay = 150 * time.Millisecond
	// queryTimeout bounds the synchronous Query made from the loop.
	queryTimeout = 10 * time.Second
	// displayCallTimeout bounds Get/Set on Mutter from helper goroutines.
	displayCallTimeout = 10 * time.Second
	watchdogInterval   = 30 * time.Second
	displayBackoffMin  = time.Second
	displayBackoffMax  = 30 * time.Second
	eventQueueSize     = 256
)

// App is the daemon. Build it with New, drive it with Run.
type App struct {
	cfg  *config.Config
	t    ha.Topics
	deps Deps
	log  *slog.Logger

	mq        MQTTClient
	sup       map[config.Scope]*systemd.Supervisor
	busOnline map[config.Scope]bool

	dispMu     sync.Mutex
	disp       display.Interface
	dispOnline bool
	dispMode   display.Mode

	st          *State
	events      chan event
	workers     map[string]*unitWorker
	tmplWorkers map[string]*templateWorker
	pending     map[config.Scope]bool
	fullPublish *time.Timer
	startedAt   time.Time
	hostname    string
	last        map[string][]byte
	force       bool

	// wakePending keeps at most one evWake per scope in the queue: a crash
	// looping unit can otherwise fill it with wakes and crowd out the events
	// that are not recoverable from a query (see pollTick).
	wakePending map[config.Scope]*atomic.Bool
	// fullPublishWanted is raised by the MQTT connect callback and only lowered
	// once the republish actually ran, so a dropped evMQTTConnected is caught up
	// by the next poll tick instead of leaving R/availability at offline.
	fullPublishWanted  atomic.Bool
	fullPublishArmedAt time.Time

	ctx      context.Context
	cancel   context.CancelFunc
	started  atomic.Bool
	loopDone chan struct{}
	wg       sync.WaitGroup
}

// New checks the dependencies and builds the topic layout. Config is assumed
// valid already; config.Load is where config errors come from.
func New(cfg *config.Config, deps Deps) (*App, error) {
	if cfg == nil {
		return nil, errors.New("app: nil config")
	}
	if deps.Systemd == nil {
		return nil, errors.New("app: deps.Systemd is required")
	}
	if deps.MQTT == nil {
		return nil, errors.New("app: deps.MQTT is required")
	}
	if deps.Log == nil {
		return nil, errors.New("app: deps.Log is required")
	}
	if cfg.DisplayEnabled() && deps.Display == nil {
		deps.Log.Warn("display enabled in config but no display dialer given, display entities stay unavailable")
	}
	host, _ := os.Hostname()
	wakePending := map[config.Scope]*atomic.Bool{}
	for _, scope := range cfg.Scopes() {
		wakePending[scope] = new(atomic.Bool)
	}
	return &App{
		cfg:         cfg,
		t:           ha.NewTopics(cfg),
		deps:        deps,
		log:         deps.Log,
		sup:         map[config.Scope]*systemd.Supervisor{},
		busOnline:   map[config.Scope]bool{},
		dispMode:    display.ModeUnknown,
		st:          NewState(cfg),
		events:      make(chan event, eventQueueSize),
		workers:     map[string]*unitWorker{},
		tmplWorkers: map[string]*templateWorker{},
		pending:     map[config.Scope]bool{},
		hostname:    host,
		last:        map[string][]byte{},
		loopDone:    make(chan struct{}),
		wakePending: wakePending,
	}, nil
}

// Run connects everything and runs the event loop until ctx is done or a
// daemon restart is requested. It returns nil on a clean stop and an error
// only when MQTT cannot even be set up; a missing bus, broker or Mutter is
// retried forever and never ends Run.
func (a *App) Run(parent context.Context) error {
	if !a.started.CompareAndSwap(false, true) {
		return errors.New("app: Run called twice")
	}
	ctx, cancel := context.WithCancel(parent)
	a.ctx, a.cancel = ctx, cancel
	defer cancel()
	defer close(a.loopDone)
	a.startedAt = time.Now()

	for _, scope := range a.cfg.Scopes() {
		var names []string
		for _, u := range a.cfg.UnitsForScope(scope) {
			names = append(names, u.Name)
		}
		sup := systemd.NewSupervisor(scope, names, a.deps.Systemd)
		a.sup[scope] = sup
		a.wg.Add(2)
		go func() {
			defer a.wg.Done()
			sup.Run(ctx)
		}()
		go func() {
			defer a.wg.Done()
			a.bridgeSupervisor(ctx, scope, sup)
		}()
	}

	if a.cfg.DisplayEnabled() && a.deps.Display != nil {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runDisplay(ctx)
		}()
	}

	mq, err := a.deps.MQTT(a.t.Availability(),
		func() {
			a.fullPublishWanted.Store(true)
			a.enqueue(evMQTTConnected{})
		},
		func(err error) { a.enqueue(evMQTTLost{err: err}) })
	if err != nil {
		cancel()
		a.wg.Wait()
		return fmt.Errorf("mqtt: %w", err)
	}
	a.mq = mq
	for _, filter := range a.t.Subscriptions(a.cfg) {
		mq.Subscribe(filter, a.onMessage)
	}
	if err := mq.Connect(); err != nil {
		cancel()
		a.wg.Wait()
		return fmt.Errorf("mqtt connect: %w", err)
	}
	// Ready means "subscriptions registered and connect kicked off", not
	// "broker answered": the broker may be down and we still want to be up.
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	a.log.Info("started", "version", a.version(), "node_id", a.cfg.NodeID, "scopes", a.cfg.Scopes())

	poll := time.NewTicker(a.cfg.Systemd.PollInterval)
	defer poll.Stop()
	watchdog := time.NewTicker(watchdogInterval)
	defer watchdog.Stop()

	for {
		select {
		case ev := <-a.events:
			a.handle(ev)
		case <-poll.C:
			a.pollTick()
		case <-watchdog.C:
			_, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog)
		case <-ctx.Done():
			a.shutdown()
			return nil
		}
	}
}

// enqueue never blocks. A full queue means something is badly wrong upstream;
// dropping an event is safe because queries, not events, are the truth and the
// poll ticker catches up, including on bus availability and the post-connect
// republish, which pollTick resynchronises from their sources.
func (a *App) enqueue(ev event) {
	select {
	case a.events <- ev:
	default:
		a.log.Warn("event queue full, dropping event", "event", fmt.Sprintf("%T", ev))
	}
}

// onMessage is the MQTT subscription handler; it must not block.
func (a *App) onMessage(topic string, payload []byte, retained bool) {
	p := make([]byte, len(payload))
	copy(p, payload)
	a.enqueue(evInbound{topic: topic, payload: p, retained: retained})
}

// bridgeSupervisor turns one supervisor's channels into events.
func (a *App) bridgeSupervisor(ctx context.Context, scope config.Scope, sup *systemd.Supervisor) {
	for {
		select {
		case online := <-sup.StateChanges():
			a.enqueue(evBusState{scope: scope, online: online})
		case <-sup.Wake():
			if a.wakePending[scope].CompareAndSwap(false, true) {
				a.enqueue(evWake{scope: scope})
			}
		case <-ctx.Done():
			return
		}
	}
}

// handle is the event dispatcher. Everything in here runs on the loop goroutine.
func (a *App) handle(ev event) {
	switch e := ev.(type) {
	case evWake:
		if p, ok := a.wakePending[e.scope]; ok {
			p.Store(false)
		}
		a.scheduleReconcile(e.scope)
	case evReconcile:
		a.reconcileScope(e.scope)
	case evBusState:
		a.busOnline[e.scope] = e.online
		if e.online {
			a.log.Info("bus online", "scope", string(e.scope))
		} else {
			a.log.Warn("bus offline", "scope", string(e.scope))
		}
		a.publishBus(e.scope)
		a.publishDaemonState()
		if e.online {
			a.reconcileScope(e.scope)
		}
	case evDisplayMode:
		a.applyDisplay(e.mode)
	case evDisplayError:
		if a.dispOnline {
			a.log.Warn("display offline", "err", e.err)
		}
		a.dispOnline = false
		a.publishDisplay()
		a.publishDaemonState()
	case evMQTTConnected:
		a.log.Info("mqtt connected", "reconnects", a.mq.Reconnects())
		a.requestFullPublish()
	case evMQTTLost:
		a.log.Warn("mqtt connection lost", "err", e.err)
	case evInbound:
		a.handleInbound(e)
	case evJobDone:
		a.handleJobDone(e)
	case evTemplateDone:
		if e.err != nil {
			a.log.Warn("template select failed", "template", e.id, "label", e.label, "err", e.err)
		} else {
			a.log.Info("template select done", "template", e.id, "label", e.label)
		}
		if sel, ok := a.selectByID(e.id); ok {
			a.scheduleReconcile(sel.Scope)
		}
	case evFullPublish:
		a.fullPublishArmedAt = time.Time{}
		a.fullPublishWanted.Store(false)
		a.republishAll()
		if e.done != nil {
			close(e.done)
		}
	default:
		a.log.Warn("unknown event", "event", fmt.Sprintf("%T", ev))
	}
}

// pollTick is the periodic truth check: query every scope and re-read the
// display. It also repairs the two states a dropped event would otherwise leave
// wrong for good: bus availability is re-read from the supervisor, and a
// republish the connect callback asked for but never got is requested again.
func (a *App) pollTick() {
	if a.fullPublishWanted.Load() {
		if a.fullPublishArmedAt.IsZero() || time.Since(a.fullPublishArmedAt) > 2*fullPublishDelay {
			a.log.Warn("republish request was lost, requesting it again")
			a.requestFullPublish()
		}
	}
	for _, scope := range a.cfg.Scopes() {
		if sup, ok := a.sup[scope]; ok {
			if on := sup.Online(); on != a.busOnline[scope] {
				a.log.Warn("bus state event was lost, resynchronising", "scope", string(scope), "online", on)
				// handle publishes the availability and reconciles when online.
				a.handle(evBusState{scope: scope, online: on})
				continue
			}
		}
		a.reconcileScope(scope)
	}
	a.refreshDisplay()
}

// requestFullPublish (re)arms the coalescing timer.
func (a *App) requestFullPublish() {
	a.fullPublishArmedAt = time.Now()
	if a.fullPublish == nil {
		a.fullPublish = time.AfterFunc(fullPublishDelay, func() { a.enqueue(evFullPublish{}) })
		return
	}
	a.fullPublish.Reset(fullPublishDelay)
}

// RepublishAll publishes discovery, every cached state and availability, in
// that order. While Run is going it hands the work to the loop and waits for
// it, so tests and callers never race with the loop.
func (a *App) RepublishAll() {
	if !a.started.Load() {
		a.republishAll()
		return
	}
	done := make(chan struct{})
	select {
	case a.events <- evFullPublish{done: done}:
	case <-a.loopDone:
		return
	}
	select {
	case <-done:
	case <-a.loopDone:
	}
}

// shutdown runs on the loop goroutine once the context is done.
func (a *App) shutdown() {
	a.log.Info("shutting down")
	if a.fullPublish != nil {
		a.fullPublish.Stop()
	}
	if a.mq != nil {
		a.mq.Close(a.t.Availability())
	}
	// Supervisors close their managers and the display runner closes Mutter
	// on ctx.Done; wait so nothing is left half open when Run returns.
	a.wg.Wait()
}

func (a *App) version() string {
	if a.deps.Version != "" {
		return a.deps.Version
	}
	return version.Version
}

func (a *App) commit() string {
	if a.deps.Commit != "" {
		return a.deps.Commit
	}
	return version.Commit
}

func (a *App) selectByID(id string) (config.TemplateSelect, bool) {
	for _, sel := range a.cfg.Selects() {
		if sel.ID == id {
			return sel, true
		}
	}
	return config.TemplateSelect{}, false
}

// Display plumbing. The controller pointer is shared between the runner, the
// loop and command goroutines, hence the mutex; the mode itself is loop-owned.

func (a *App) display() display.Interface {
	a.dispMu.Lock()
	defer a.dispMu.Unlock()
	return a.disp
}

func (a *App) setDisplay(d display.Interface) {
	a.dispMu.Lock()
	defer a.dispMu.Unlock()
	a.disp = d
}

// runDisplay dials Mutter with backoff, reads the mode once, then forwards
// changes until the connection dies and starts over.
func (a *App) runDisplay(ctx context.Context) {
	backoff := displayBackoffMin
	wait := func() bool {
		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, displayBackoffMax)
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		d, err := a.deps.Display(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("display unavailable", "err", err, "retry_in", backoff.String())
			a.enqueue(evDisplayError{err: err})
			if !wait() {
				return
			}
			continue
		}
		a.setDisplay(d)

		getCtx, cancel := context.WithTimeout(ctx, displayCallTimeout)
		m, err := d.Get(getCtx)
		cancel()
		if err != nil {
			a.setDisplay(nil)
			d.Close()
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("display read failed", "err", err, "retry_in", backoff.String())
			a.enqueue(evDisplayError{err: err})
			if !wait() {
				return
			}
			continue
		}
		backoff = displayBackoffMin
		a.enqueue(evDisplayMode{mode: m})

		alive := true
		for alive {
			select {
			case m, ok := <-d.Changes():
				if !ok {
					alive = false
					a.enqueue(evDisplayError{err: errors.New("display change channel closed")})
					break
				}
				a.enqueue(evDisplayMode{mode: m})
			case err := <-d.Errors():
				alive = false
				a.enqueue(evDisplayError{err: err})
			case <-ctx.Done():
				a.setDisplay(nil)
				d.Close()
				return
			}
		}
		a.setDisplay(nil)
		d.Close()
		if !wait() {
			return
		}
	}
}

// refreshDisplay re-reads the mode from a helper goroutine; the answer comes back as an event.
func (a *App) refreshDisplay() {
	d := a.display()
	if d == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, displayCallTimeout)
		defer cancel()
		m, err := d.Get(ctx)
		if err != nil {
			a.enqueue(evDisplayError{err: err})
			return
		}
		a.enqueue(evDisplayMode{mode: m})
	}()
}

// applyDisplay records a mode and publishes it. Mode -1 counts as unavailable.
func (a *App) applyDisplay(m display.Mode) {
	if !a.dispOnline || m != a.dispMode {
		a.log.Info("display mode", "mode", m.String(), "power_save_mode", int32(m))
	}
	a.dispOnline = true
	a.dispMode = m
	a.publishDisplay()
	a.publishDaemonState()
}

// displayAvailable is what the display availability topic says.
func (a *App) displayAvailable() bool {
	return a.dispOnline && a.dispMode != display.ModeUnknown
}
