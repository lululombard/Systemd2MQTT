package systemd

import (
	"context"
	"fmt"
	"sync"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// FakeManager is an in-memory Manager for tests in this and other packages.
// Statuses are settable, Wake can be triggered by hand, and errors can be
// queued per call. A job that succeeds also updates the unit's state and
// triggers a wake, the way a real manager would emit PropertiesChanged.
type FakeManager struct {
	mu       sync.Mutex
	scope    config.Scope
	statuses map[string]UnitStatus
	queryErr []error
	jobs     map[string]fakeJob
	calls    []string
	closed   bool
	wake     chan struct{}
}

type fakeJob struct {
	result JobResult
	err    error
}

// NewFakeManager returns an empty fake; every queried unit is not-found until SetStatus.
func NewFakeManager(scope config.Scope) *FakeManager {
	return &FakeManager{
		scope:    scope,
		statuses: map[string]UnitStatus{},
		jobs:     map[string]fakeJob{},
		wake:     make(chan struct{}, 1),
	}
}

// SetStatus sets what Query reports for a unit.
func (f *FakeManager) SetStatus(name, active, sub, load string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[name] = UnitStatus{Name: name, Scope: f.scope, ActiveState: active, SubState: sub, LoadState: load}
}

// FailNextQuery queues an error for the next Query call. Call it twice to fail twice.
func (f *FakeManager) FailNextQuery(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryErr = append(f.queryErr, err)
}

// SetJobResult fixes what Start/Stop/Restart return for a unit. Without it jobs are "done".
// A non-nil err makes the call itself fail (result JobError), like a polkit denial.
func (f *FakeManager) SetJobResult(name string, result JobResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[name] = fakeJob{result: result, err: err}
}

// TriggerWake sends one coalesced wake hint.
func (f *FakeManager) TriggerWake() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Calls lists the jobs run so far as "verb name mode".
func (f *FakeManager) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Closed reports whether Close was called.
func (f *FakeManager) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *FakeManager) Scope() config.Scope   { return f.scope }
func (f *FakeManager) Wake() <-chan struct{} { return f.wake }

func (f *FakeManager) Query(_ context.Context, names []string) ([]UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queryErr) > 0 {
		err := f.queryErr[0]
		f.queryErr = f.queryErr[1:]
		return nil, err
	}
	out := make([]UnitStatus, 0, len(names))
	for _, n := range names {
		if st, ok := f.statuses[n]; ok {
			out = append(out, st)
			continue
		}
		out = append(out, UnitStatus{Name: n, Scope: f.scope, ActiveState: "inactive", SubState: "dead", LoadState: "not-found"})
	}
	return out, nil
}

func (f *FakeManager) Start(_ context.Context, name, mode string) (JobResult, error) {
	return f.job("start", name, mode, "active", "running")
}

func (f *FakeManager) Stop(_ context.Context, name, mode string) (JobResult, error) {
	return f.job("stop", name, mode, "inactive", "dead")
}

func (f *FakeManager) Restart(_ context.Context, name, mode string) (JobResult, error) {
	return f.job("restart", name, mode, "active", "running")
}

func (f *FakeManager) job(verb, name, mode, active, sub string) (JobResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s", verb, name, mode))
	j, ok := f.jobs[name]
	if !ok {
		j = fakeJob{result: JobDone}
	}
	if j.err != nil {
		f.mu.Unlock()
		return JobError, j.err
	}
	if j.result == JobDone {
		st := f.statuses[name]
		st.Name, st.Scope = name, f.scope
		st.ActiveState, st.SubState = active, sub
		if st.LoadState == "" {
			st.LoadState = "loaded"
		}
		f.statuses[name] = st
	}
	f.mu.Unlock()
	if j.result == JobDone {
		f.TriggerWake()
	}
	return j.result, nil
}

func (f *FakeManager) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// FakeDialer hands out FakeManagers. It can fail the first few dials and
// seed each new manager through OnDial.
type FakeDialer struct {
	mu        sync.Mutex
	FailFirst int                // number of dials that fail before the first success
	OnDial    func(*FakeManager) // optional, runs before the manager is returned
	dials     int
	managers  []*FakeManager
}

// Dial matches the Dialer type.
func (d *FakeDialer) Dial(_ context.Context, scope config.Scope, _ []string) (Manager, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.dials <= d.FailFirst {
		return nil, fmt.Errorf("fake dial %d failed", d.dials)
	}
	m := NewFakeManager(scope)
	if d.OnDial != nil {
		d.OnDial(m)
	}
	d.managers = append(d.managers, m)
	return m, nil
}

// Dials counts every attempt, failed ones included.
func (d *FakeDialer) Dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// Last returns the most recently dialed manager, or nil.
func (d *FakeDialer) Last() *FakeManager {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.managers) == 0 {
		return nil
	}
	return d.managers[len(d.managers)-1]
}
