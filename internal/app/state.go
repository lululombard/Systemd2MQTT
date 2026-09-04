package app

import (
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
)

// UnitSnapshot is the event loop's view of one unit: the last query result
// plus the outcome of the last job we ran on it. Nothing in here is optimistic,
// ActiveState only ever comes from a query.
type UnitSnapshot struct {
	ID    string
	Name  string
	Scope config.Scope

	// Known is false until the first successful query for this unit.
	Known       bool
	ActiveState string
	SubState    string
	LoadState   string
	// ChangedAt bumps only when active, sub or load state actually changed.
	ChangedAt time.Time

	LastJob       string
	LastJobResult string
	LastJobError  string
	LastJobAt     time.Time
}

// IsOn mirrors systemd.UnitStatus.IsOn for the switch payload.
func (s UnitSnapshot) IsOn() bool {
	return systemd.UnitStatus{ActiveState: s.ActiveState}.IsOn()
}

// State is everything the loop publishes from. Only the loop goroutine touches it.
type State struct {
	order  []string
	units  map[string]*UnitSnapshot
	byName map[config.Scope]map[string]string
}

// NewState seeds one empty snapshot per configured unit, in config order.
func NewState(cfg *config.Config) *State {
	s := &State{
		units:  map[string]*UnitSnapshot{},
		byName: map[config.Scope]map[string]string{},
	}
	for _, u := range cfg.AllUnits() {
		id := u.ObjectID()
		s.order = append(s.order, id)
		s.units[id] = &UnitSnapshot{ID: id, Name: u.Name, Scope: u.Scope}
		if s.byName[u.Scope] == nil {
			s.byName[u.Scope] = map[string]string{}
		}
		s.byName[u.Scope][u.Name] = id
	}
	return s
}

// IDForUnit maps a queried unit name back to its object id.
func (s *State) IDForUnit(scope config.Scope, name string) (string, bool) {
	id, ok := s.byName[scope][name]
	return id, ok
}

// ApplyUnit records a query result. changed is true on the first result and
// whenever active, sub or load state moved; only then does ChangedAt bump.
func (s *State) ApplyUnit(scope config.Scope, st systemd.UnitStatus, now time.Time) (snap UnitSnapshot, changed, ok bool) {
	id, ok := s.IDForUnit(scope, st.Name)
	if !ok {
		return UnitSnapshot{}, false, false
	}
	u := s.units[id]
	if !u.Known || u.ActiveState != st.ActiveState || u.SubState != st.SubState || u.LoadState != st.LoadState {
		u.Known = true
		u.ActiveState = st.ActiveState
		u.SubState = st.SubState
		u.LoadState = st.LoadState
		u.ChangedAt = now
		changed = true
	}
	return *u, changed, true
}

// SetJobResult stores what the last Start/Stop/Restart did. The switch state
// is untouched on purpose; the next query decides that.
func (s *State) SetJobResult(id, verb string, result systemd.JobResult, err error, now time.Time) (UnitSnapshot, bool) {
	u, ok := s.units[id]
	if !ok {
		return UnitSnapshot{}, false
	}
	u.LastJob = verb
	u.LastJobResult = string(result)
	u.LastJobError = ""
	if err != nil {
		u.LastJobError = err.Error()
	}
	u.LastJobAt = now
	return *u, true
}

// Unit returns one snapshot by id.
func (s *State) Unit(id string) (UnitSnapshot, bool) {
	u, ok := s.units[id]
	if !ok {
		return UnitSnapshot{}, false
	}
	return *u, true
}

// Units returns every snapshot in config order.
func (s *State) Units() []UnitSnapshot {
	out := make([]UnitSnapshot, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.units[id])
	}
	return out
}

// TemplateState derives the select state of an exclusive template from its
// instances: the label of the single active one, "off" when none. active
// lists every active label so the caller can warn when there are several.
func (s *State) TemplateState(sel config.TemplateSelect) (label string, active []string, known bool) {
	for _, opt := range sel.Options {
		u, ok := s.units[opt.UnitID]
		if !ok || !u.Known {
			continue
		}
		known = true
		if u.IsOn() {
			active = append(active, opt.Label)
		}
	}
	if len(active) == 0 {
		return "off", nil, known
	}
	return active[0], active, known
}

// Events. Everything outside the loop only ever enqueues one of these.

type event any

// evWake is a coalesced hint from a supervisor that a watched unit may have changed.
type evWake struct{ scope config.Scope }

// evReconcile fires after the wake debounce; the loop queries the scope.
type evReconcile struct{ scope config.Scope }

// evBusState is a supervisor going online or offline.
type evBusState struct {
	scope  config.Scope
	online bool
}

// evDisplayMode is a PowerSaveMode read or signalled by Mutter.
type evDisplayMode struct{ mode display.Mode }

// evDisplayError means the display connection is gone or a read failed.
type evDisplayError struct{ err error }

type evMQTTConnected struct{}

type evMQTTLost struct{ err error }

// evInbound is a message on one of the subscribed topics.
type evInbound struct {
	topic    string
	payload  []byte
	retained bool
}

// evJobDone is the outcome of one Start/Stop/Restart run by a worker.
type evJobDone struct {
	id     string
	verb   string
	result systemd.JobResult
	err    error
}

// evTemplateDone is the outcome of a template select sequence.
type evTemplateDone struct {
	id    string
	label string
	err   error
}

// evFullPublish asks the loop to republish everything; done, when set, is
// closed afterwards so RepublishAll can block on it.
type evFullPublish struct{ done chan struct{} }
