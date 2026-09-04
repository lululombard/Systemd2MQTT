package systemd

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// Supervisor keeps one Manager per scope alive across bus restarts.
//
// Liveness comes from Query, not from the connection. go-systemd's dispatcher
// goroutine exits silently when the bus closes and neither errCh nor the
// wake channel ever says "gone", so the only way to notice a dead bus is a
// failing query. The app's poll ticker calls Query every poll_interval, which
// doubles as the ping: a closed-connection error, or two failures in a row,
// requests a redial and Run reconnects with backoff.
type Supervisor struct {
	scope config.Scope
	names []string
	dial  Dialer
	log   *slog.Logger

	// backoff bounds, overridable in tests
	backoffMin time.Duration
	backoffMax time.Duration

	mu       sync.Mutex
	cur      Manager
	online   bool
	failures int
	gen      uint64 // bumped on every successful dial, so stale Query results do not count

	redial  chan struct{}
	stateCh chan bool
	wake    chan struct{}
}

// NewSupervisor builds a Supervisor for one scope. Nothing connects until Run.
func NewSupervisor(scope config.Scope, names []string, dial Dialer) *Supervisor {
	return &Supervisor{
		scope:      scope,
		names:      names,
		dial:       dial,
		log:        slog.Default().With("scope", string(scope)),
		backoffMin: time.Second,
		backoffMax: 30 * time.Second,
		redial:     make(chan struct{}, 1),
		stateCh:    make(chan bool, 4),
		wake:       make(chan struct{}, 1),
	}
}

// StateChanges reports true after a successful dial and false when the
// connection is dropped. Sends are non-blocking on a small buffer, so a slow
// reader only loses intermediate flaps, never the final state.
func (s *Supervisor) StateChanges() <-chan bool { return s.stateCh }

// Wake is the current manager's wake hint, forwarded and coalesced across reconnects.
func (s *Supervisor) Wake() <-chan struct{} { return s.wake }

// Online reports whether a manager is currently connected.
func (s *Supervisor) Online() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online
}

// Manager returns the current manager and whether it is online. The manager
// may go away at any time; callers must be ready for a disconnect error.
func (s *Supervisor) Manager() (Manager, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.online
}

// Run dials, waits for a redial request or the context, closes, and repeats
// until the context is done. It never returns early because of a bus error.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.backoffMin
	for {
		m, err := s.dial(ctx, s.scope, s.names)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("systemd manager unavailable", "err", err, "retry_in", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, s.backoffMax)
			continue
		}
		backoff = s.backoffMin

		s.mu.Lock()
		s.cur = m
		s.online = true
		s.failures = 0
		s.gen++
		// A redial queued by a Query against the previous manager is stale now.
		select {
		case <-s.redial:
		default:
		}
		s.mu.Unlock()
		s.log.Info("systemd manager online")
		s.notify(true)

		genDone := make(chan struct{})
		go s.forwardWake(m.Wake(), genDone)

		select {
		case <-s.redial:
			s.log.Warn("systemd manager unresponsive, reconnecting")
		case <-ctx.Done():
		}

		s.mu.Lock()
		s.cur = nil
		s.online = false
		s.mu.Unlock()
		close(genDone)
		s.notify(false)
		m.Close()
		s.log.Info("systemd manager offline")

		if ctx.Err() != nil {
			return
		}
	}
}

// forwardWake copies one manager's wake hints into the supervisor's own
// coalescing channel until the generation ends.
func (s *Supervisor) forwardWake(in <-chan struct{}, genDone <-chan struct{}) {
	for {
		select {
		case _, ok := <-in:
			if !ok {
				return
			}
			select {
			case s.wake <- struct{}{}:
			default:
			}
		case <-genDone:
			return
		}
	}
}

func (s *Supervisor) notify(online bool) {
	select {
	case s.stateCh <- online:
	default:
		s.log.Warn("state change dropped, reader too slow", "online", online)
	}
}

func (s *Supervisor) requestRedial() {
	select {
	case s.redial <- struct{}{}:
	default:
	}
}

// Query asks the current manager for the watched units and tracks liveness.
// With no manager it returns ErrOffline. A closed-connection error or two
// consecutive failures request a redial.
func (s *Supervisor) Query(ctx context.Context) ([]UnitStatus, error) {
	s.mu.Lock()
	m, gen := s.cur, s.gen
	s.mu.Unlock()
	if m == nil {
		return nil, ErrOffline
	}

	st, err := m.Query(ctx, s.names)

	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		// The manager was replaced while we were talking to the old one.
		return st, err
	}
	if err == nil {
		s.failures = 0
		return st, nil
	}
	s.failures++
	if IsDisconnectErr(err) || s.failures >= 2 {
		s.log.Warn("systemd query failed, requesting reconnect", "err", err, "failures", s.failures)
		s.failures = 0
		s.requestRedial()
	} else {
		s.log.Debug("systemd query failed", "err", err, "failures", s.failures)
	}
	return nil, err
}
