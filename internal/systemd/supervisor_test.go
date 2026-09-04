package systemd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

var testNames = []string{"grafana-kiosk.service", "vlc@cam1.service"}

func newTestSupervisor(t *testing.T, d *FakeDialer) (*Supervisor, context.CancelFunc) {
	t.Helper()
	s := NewSupervisor(config.ScopeUser, testNames, d.Dial)
	s.backoffMin = time.Millisecond
	s.backoffMax = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return s, cancel
}

func waitState(t *testing.T, s *Supervisor, want bool) {
	t.Helper()
	select {
	case got := <-s.StateChanges():
		if got != want {
			t.Fatalf("StateChanges sent %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no StateChanges %v within 2s", want)
	}
}

func TestSupervisorDialBackoff(t *testing.T) {
	d := &FakeDialer{FailFirst: 2, OnDial: func(m *FakeManager) {
		m.SetStatus("grafana-kiosk.service", "active", "running", "loaded")
	}}
	s, _ := newTestSupervisor(t, d)

	// Run may already have connected on a fast machine, so only nil or ErrOffline is acceptable here.
	if _, err := s.Query(context.Background()); err != nil && !errors.Is(err, ErrOffline) {
		t.Fatalf("unexpected error before connect: %v", err)
	}

	waitState(t, s, true)
	if got := d.Dials(); got != 3 {
		t.Fatalf("dials = %d, want 3 (two failures then success)", got)
	}
	if !s.Online() {
		t.Fatal("Online() = false after connect")
	}
	m, ok := s.Manager()
	if !ok || m == nil {
		t.Fatal("Manager() should return the current manager")
	}

	st, err := s.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || !st[0].IsOn() || !st[1].IsMissing() {
		t.Fatalf("unexpected statuses: %+v", st)
	}
}

func TestSupervisorRedialAfterTwoFailures(t *testing.T) {
	d := &FakeDialer{}
	s, _ := newTestSupervisor(t, d)
	waitState(t, s, true)
	first := d.Last()

	boom := errors.New("boom")
	first.FailNextQuery(boom)
	first.FailNextQuery(boom)

	if _, err := s.Query(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("first Query err = %v, want boom", err)
	}
	// One plain failure is not enough to redial.
	select {
	case v := <-s.StateChanges():
		t.Fatalf("unexpected state change %v after one failure", v)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := s.Query(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("second Query err = %v, want boom", err)
	}

	waitState(t, s, false)
	waitState(t, s, true)
	if !first.Closed() {
		t.Error("old manager was not closed")
	}
	if d.Dials() != 2 {
		t.Errorf("dials = %d, want 2", d.Dials())
	}
	if d.Last() == first {
		t.Fatal("expected a fresh manager after redial")
	}
	if _, err := s.Query(context.Background()); err != nil {
		t.Fatalf("Query on the new manager failed: %v", err)
	}
}

func TestSupervisorRedialOnDisconnectErr(t *testing.T) {
	d := &FakeDialer{}
	s, _ := newTestSupervisor(t, d)
	waitState(t, s, true)
	first := d.Last()

	first.FailNextQuery(dbus.ErrClosed)
	if _, err := s.Query(context.Background()); !errors.Is(err, dbus.ErrClosed) {
		t.Fatalf("Query err = %v", err)
	}
	waitState(t, s, false)
	waitState(t, s, true)
	if !first.Closed() || d.Dials() != 2 {
		t.Fatalf("closed=%v dials=%d", first.Closed(), d.Dials())
	}
}

func TestSupervisorQueryOfflineAndSuccessResetsFailures(t *testing.T) {
	d := &FakeDialer{}
	s := NewSupervisor(config.ScopeUser, testNames, d.Dial)
	if _, err := s.Query(context.Background()); !errors.Is(err, ErrOffline) {
		t.Fatalf("Query before Run err = %v, want ErrOffline", err)
	}
	if _, ok := s.Manager(); ok || s.Online() {
		t.Fatal("should be offline before Run")
	}

	s, _ = newTestSupervisor(t, d)
	waitState(t, s, true)
	m := d.Last()

	// fail, succeed, fail: the success resets the counter so no redial happens.
	m.FailNextQuery(errors.New("one"))
	if _, err := s.Query(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if _, err := s.Query(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.FailNextQuery(errors.New("two"))
	if _, err := s.Query(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	select {
	case v := <-s.StateChanges():
		t.Fatalf("unexpected state change %v", v)
	case <-time.After(30 * time.Millisecond):
	}
	if d.Dials() != 1 {
		t.Fatalf("dials = %d, want 1", d.Dials())
	}
}

func TestSupervisorWakeForwardingCoalesces(t *testing.T) {
	d := &FakeDialer{}
	s, _ := newTestSupervisor(t, d)
	waitState(t, s, true)
	m := d.Last()

	for range 5 {
		m.TriggerWake()
	}
	// Give the forwarder time to drain everything into the cap-1 channel.
	time.Sleep(50 * time.Millisecond)

	got := 0
	for {
		select {
		case <-s.Wake():
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Fatalf("got %d wakes, want exactly 1 (coalesced)", got)
	}

	// A job on the fake also wakes, through the same forwarder.
	if r, err := m.Start(context.Background(), "vlc@cam1.service", "replace"); err != nil || r != JobDone {
		t.Fatalf("Start = %v, %v", r, err)
	}
	select {
	case <-s.Wake():
	case <-time.After(time.Second):
		t.Fatal("no wake after a job")
	}
	st, _ := s.Query(context.Background())
	if !st[1].IsOn() {
		t.Fatalf("fake Start did not update state: %+v", st[1])
	}
}

func TestSupervisorStopsOnContext(t *testing.T) {
	d := &FakeDialer{}
	s, cancel := newTestSupervisor(t, d)
	waitState(t, s, true)
	m := d.Last()
	cancel()
	waitState(t, s, false)
	deadline := time.Now().Add(time.Second)
	for !m.Closed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !m.Closed() {
		t.Error("manager not closed on cancel")
	}
	if s.Online() {
		t.Error("still online after cancel")
	}
}

func TestFakeManagerJobErrors(t *testing.T) {
	m := NewFakeManager(config.ScopeSystem)
	denied := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	m.SetJobResult("cron.service", JobError, denied)
	r, err := m.Start(context.Background(), "cron.service", "replace")
	if r != JobError || !IsPolkitDenied(err) {
		t.Fatalf("Start = %v, %v", r, err)
	}
	m.SetJobResult("bad.service", JobFailed, nil)
	if r, err := m.Start(context.Background(), "bad.service", "replace"); r != JobFailed || err != nil {
		t.Fatalf("Start = %v, %v", r, err)
	}
	st, _ := m.Query(context.Background(), []string{"bad.service"})
	if st[0].IsOn() {
		t.Fatal("a failed job must not flip the state")
	}
	if calls := m.Calls(); len(calls) != 2 || calls[0] != "start cron.service replace" {
		t.Fatalf("calls = %v", calls)
	}
}
