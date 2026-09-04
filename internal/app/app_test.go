package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/ha"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
)

// The office kiosk example: one plain unit and an exclusive template with two instances.
const kioskYAML = `
node_id: office-kiosk
device: { name: Office kiosk, suggested_area: Office }
mqtt:
  broker: tcp://10.2.0.1:1883
  username: systemd2mqtt
  password: "change-me"
units:
  - { name: grafana-kiosk.service, scope: user, friendly_name: Grafana kiosk, icon: mdi:monitor-dashboard }
templates:
  - name: vlc@.service
    scope: user
    friendly_name: Camera
    icon: mdi:cctv
    exclusive: true
    instances:
      - { name: cam1, friendly_name: Front door }
      - { name: cam2, friendly_name: Parking }
display:
  off_mode: off
`

// A config with a system-scope unit for the polkit and redial paths.
const systemYAML = `
node_id: office-kiosk
mqtt:
  broker: tcp://10.2.0.1:1883
units:
  - { name: grafana-kiosk.service, scope: user }
  - { name: cron.service, scope: system }
`

const (
	grafana = "grafana-kiosk.service"
	cam1    = "vlc@cam1.service"
	cam2    = "vlc@cam2.service"
)

const waitTimeout = 5 * time.Second

func init() {
	// Keep the tests fast; the real delays are only about coalescing.
	fullPublishDelay = 60 * time.Millisecond
	reconcileDelay = 10 * time.Millisecond
	displayBackoffMin = 10 * time.Millisecond
	displayBackoffMax = 50 * time.Millisecond
}

type harness struct {
	t      *testing.T
	app    *App
	cfg    *config.Config
	topics ha.Topics
	mq     *fakeMQTT
	dialer *systemd.FakeDialer
	disp   *display.FakeController
	cancel context.CancelFunc
	done   chan error

	mu       sync.Mutex
	managers map[config.Scope]*systemd.FakeManager
}

type harnessOpts struct {
	yaml        string
	displayMode display.Mode
	noDisplay   bool
	seed        func(*systemd.FakeManager)
}

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// start builds an App on fakes and runs it until the test ends.
func start(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.yaml == "" {
		o.yaml = kioskYAML
	}
	cfg, err := config.Parse([]byte(o.yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	h := &harness{
		t:        t,
		cfg:      cfg,
		topics:   ha.NewTopics(cfg),
		mq:       newFakeMQTT(),
		done:     make(chan error, 1),
		managers: map[config.Scope]*systemd.FakeManager{},
	}
	h.dialer = &systemd.FakeDialer{OnDial: func(m *systemd.FakeManager) {
		if o.seed != nil {
			o.seed(m)
		}
		h.mu.Lock()
		h.managers[m.Scope()] = m
		h.mu.Unlock()
	}}
	deps := Deps{
		Systemd: h.dialer.Dial,
		MQTT:    h.mq.factory,
		Log:     testLogger(),
		Version: "1.2.3",
		Commit:  "abc1234",
	}
	if !o.noDisplay {
		h.disp = display.NewFake(o.displayMode)
		deps.Display = func(context.Context) (display.Interface, error) { return h.disp, nil }
	}
	h.app, err = New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(waitTimeout):
			t.Errorf("Run did not return after cancel")
		}
	})
	return h
}

// waitFor polls cond until it holds or the timeout hits.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// waitOnline waits for the startup republish to finish (availability online is its last step).
func (h *harness) waitOnline() {
	h.t.Helper()
	h.waitFor("availability online", func() bool {
		last, ok := h.mq.Last(h.topics.Availability())
		return ok && last == "online"
	})
}

func (h *harness) waitLast(topic, want string) {
	h.t.Helper()
	h.waitFor(topic+" = "+want, func() bool {
		last, ok := h.mq.Last(topic)
		return ok && last == want
	})
}

// manager waits for the current user-scope manager.
func (h *harness) manager() *systemd.FakeManager { return h.managerFor(config.ScopeUser) }

// managerFor waits for the most recently dialed manager of a scope.
func (h *harness) managerFor(scope config.Scope) *systemd.FakeManager {
	h.t.Helper()
	var m *systemd.FakeManager
	h.waitFor("a dialed "+string(scope)+" manager", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		m = h.managers[scope]
		return m != nil
	})
	return m
}

func (h *harness) attributes(id string) map[string]any {
	h.t.Helper()
	raw, ok := h.mq.Last(h.topics.UnitAttributes(id))
	if !ok {
		h.t.Fatalf("no attributes published for %s", id)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		h.t.Fatalf("attributes for %s are not JSON: %v", id, err)
	}
	return m
}

func seedKiosk(m *systemd.FakeManager) {
	m.SetStatus(grafana, "active", "running", "loaded")
	m.SetStatus(cam1, "inactive", "dead", "loaded")
	m.SetStatus(cam2, "inactive", "dead", "loaded")
}

func indexOf(pubs []fakePub, topic string) int {
	for i, p := range pubs {
		if p.topic == topic {
			return i
		}
	}
	return -1
}

func TestNewRejectsMissingDeps(t *testing.T) {
	cfg, err := config.Parse([]byte(kioskYAML))
	if err != nil {
		t.Fatal(err)
	}
	mq := newFakeMQTT()
	dialer := &systemd.FakeDialer{}
	cases := map[string]Deps{
		"systemd": {MQTT: mq.factory, Log: testLogger()},
		"mqtt":    {Systemd: dialer.Dial, Log: testLogger()},
		"log":     {Systemd: dialer.Dial, MQTT: mq.factory},
	}
	for name, deps := range cases {
		if _, err := New(cfg, deps); err == nil {
			t.Errorf("New without %s dep should fail", name)
		}
	}
	if _, err := New(cfg, Deps{Systemd: dialer.Dial, MQTT: mq.factory, Log: testLogger()}); err != nil {
		t.Errorf("New without display should work: %v", err)
	}
}

func TestStartupPublishesInOrder(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()

	pubs := h.mq.Publishes()
	disc := indexOf(pubs, h.topics.DiscoveryConfig())
	if disc < 0 {
		t.Fatalf("discovery never published")
	}
	if pubs[len(pubs)-1].topic != h.topics.Availability() || pubs[len(pubs)-1].payload != "online" {
		t.Fatalf("last publish should be availability online, got %+v", pubs[len(pubs)-1])
	}
	// Every state topic is republished between discovery and availability.
	after := pubs[disc+1 : len(pubs)-1]
	want := []string{
		h.topics.BusAvailability(config.ScopeUser),
		h.topics.UnitState("grafana_kiosk"),
		h.topics.UnitAttributes("grafana_kiosk"),
		h.topics.UnitState("vlc_cam1"),
		h.topics.UnitState("vlc_cam2"),
		h.topics.TemplateState("vlc"),
		h.topics.DisplayState(),
		h.topics.DisplayModeState(),
		h.topics.DisplayAttributes(),
		h.topics.DisplayAvailability(),
		h.topics.DaemonState(),
	}
	for _, topic := range want {
		if indexOf(after, topic) < 0 {
			t.Errorf("%s not republished between discovery and availability", topic)
		}
	}
	for _, p := range pubs {
		if !p.retained {
			t.Errorf("%s published without retain", p.topic)
		}
	}
	if got, _ := h.mq.Last(h.topics.UnitState("grafana_kiosk")); got != "ON" {
		t.Errorf("grafana state = %q, want ON", got)
	}
	if got, _ := h.mq.Last(h.topics.TemplateState("vlc")); got != "off" {
		t.Errorf("template state = %q, want off", got)
	}
	if got, _ := h.mq.Last(h.topics.DisplayState()); got != "ON" {
		t.Errorf("display state = %q, want ON", got)
	}
	if got, _ := h.mq.Last(h.topics.BusAvailability(config.ScopeUser)); got != "online" {
		t.Errorf("bus availability = %q, want online", got)
	}

	var daemon map[string]any
	raw, _ := h.mq.Last(h.topics.DaemonState())
	if err := json.Unmarshal([]byte(raw), &daemon); err != nil {
		t.Fatalf("daemon state not JSON: %v", err)
	}
	if daemon["version"] != "1.2.3" || daemon["bus_user_online"] != true || daemon["display_online"] != true {
		t.Errorf("daemon state = %s", raw)
	}
	if _, has := daemon["bus_system_online"]; has {
		t.Errorf("bus_system_online must be absent without a system scope: %s", raw)
	}

	var disc2 map[string]any
	raw, _ = h.mq.Last(h.topics.DiscoveryConfig())
	if err := json.Unmarshal([]byte(raw), &disc2); err != nil {
		t.Fatalf("discovery not JSON: %v", err)
	}
	if _, ok := disc2["cmps"]; !ok {
		t.Errorf("discovery has no cmps: %s", raw)
	}

	subs := strings.Join(h.mq.Subscriptions(), " ")
	for _, want := range h.topics.Subscriptions(h.cfg) {
		if !strings.Contains(subs, want) {
			t.Errorf("not subscribed to %s (have %s)", want, subs)
		}
	}
}

func TestHABirthCoalesces(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()
	if n := h.mq.Count(h.topics.DiscoveryConfig()); n != 1 {
		t.Fatalf("discovery published %d times at startup, want 1", n)
	}

	for range 3 {
		if !h.mq.Deliver(h.cfg.HomeAssistant.StatusTopic, "online", true) {
			t.Fatal("nobody subscribed to the HA status topic")
		}
	}
	h.waitFor("second republish", func() bool { return h.mq.Count(h.topics.DiscoveryConfig()) == 2 })
	time.Sleep(4 * fullPublishDelay)
	if n := h.mq.Count(h.topics.DiscoveryConfig()); n != 2 {
		t.Fatalf("three births gave %d republishes, want 1", n-1)
	}
	pubs := h.mq.Publishes()
	if last := pubs[len(pubs)-1]; last.topic != h.topics.Availability() {
		t.Errorf("republish should end with availability, ended with %s", last.topic)
	}
}

func TestRetainedCommandIsIgnored(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()
	m := h.manager()

	h.mq.Deliver(h.topics.UnitSet("vlc_cam1"), "ON", true)
	// A live command after it proves the loop processed both, in order.
	h.mq.Deliver(h.topics.UnitSet("grafana_kiosk"), "RESTART", false)
	h.waitFor("restart call", func() bool { return len(m.Calls()) > 0 })
	calls := m.Calls()
	if len(calls) != 1 || calls[0] != "restart "+grafana+" replace" {
		t.Fatalf("calls = %v, want only the restart", calls)
	}
}

func TestUnitCommandAndFailedJob(t *testing.T) {
	h := start(t, harnessOpts{seed: func(m *systemd.FakeManager) {
		seedKiosk(m)
		m.SetJobResult(cam1, systemd.JobFailed, nil)
	}})
	h.waitOnline()
	m := h.manager()

	// A successful start flips the switch through the query, not optimistically.
	h.mq.Deliver(h.topics.UnitSet("vlc_cam2"), "ON", false)
	h.waitLast(h.topics.UnitState("vlc_cam2"), "ON")
	h.waitFor("cam2 job attributes", func() bool {
		raw, _ := h.mq.Last(h.topics.UnitAttributes("vlc_cam2"))
		return strings.Contains(raw, `"last_job_result":"done"`)
	})
	attrs := h.attributes("vlc_cam2")
	if attrs["last_job"] != "start" || attrs["active_state"] != "active" || attrs["unit"] != cam2 {
		t.Errorf("cam2 attributes = %v", attrs)
	}
	if _, has := attrs["last_job_error"]; has {
		t.Errorf("last_job_error should be omitted on success: %v", attrs)
	}
	if _, err := time.Parse(time.RFC3339, attrs["last_job_at"].(string)); err != nil {
		t.Errorf("last_job_at not RFC3339: %v", attrs["last_job_at"])
	}

	// A failed start leaves the switch OFF and shows why.
	h.mq.Deliver(h.topics.UnitSet("vlc_cam1"), "ON", false)
	h.waitFor("cam1 failed job", func() bool {
		raw, _ := h.mq.Last(h.topics.UnitAttributes("vlc_cam1"))
		return strings.Contains(raw, `"last_job_result":"failed"`)
	})
	time.Sleep(5 * reconcileDelay)
	if got, _ := h.mq.Last(h.topics.UnitState("vlc_cam1")); got != "OFF" {
		t.Errorf("cam1 state after failed start = %q, want OFF", got)
	}
	found := false
	for _, c := range m.Calls() {
		if c == "start "+cam1+" replace" {
			found = true
		}
	}
	if !found {
		t.Errorf("start never reached the manager: %v", m.Calls())
	}

	if !h.mq.Deliver(h.topics.UnitSet("vlc_cam1"), "bogus", false) {
		t.Fatal("unit set not subscribed")
	}
	h.mq.Deliver(h.topics.UnitSet("grafana_kiosk"), "OFF", false)
	h.waitLast(h.topics.UnitState("grafana_kiosk"), "OFF")
}

func TestDisplayUnknownIsUnavailable(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk, displayMode: display.ModeUnknown})
	h.waitOnline()
	h.waitLast(h.topics.DisplayAvailability(), "offline")
	if n := h.mq.Count(h.topics.DisplayState()); n != 0 {
		t.Errorf("display state published %d times with mode -1, want 0", n)
	}
	if n := h.mq.Count(h.topics.DisplayModeState()); n != 0 {
		t.Errorf("display mode published %d times with mode -1, want 0", n)
	}
	raw, _ := h.mq.Last(h.topics.DaemonState())
	if !strings.Contains(raw, `"display_online":false`) {
		t.Errorf("daemon state should say display offline: %s", raw)
	}

	// A write is refused while unknown, and the entities come back once Mutter reports a mode.
	h.mq.Deliver(h.topics.DisplaySet(), "OFF", false)
	h.waitFor("set attempted", func() bool { return len(h.disp.Sets()) == 1 })
	h.disp.SetMode(display.ModeOn)
	h.disp.PushChange(display.ModeOn)
	h.waitLast(h.topics.DisplayAvailability(), "online")
	h.waitLast(h.topics.DisplayState(), "ON")

	h.mq.Deliver(h.topics.DisplaySet(), "OFF", false)
	h.waitLast(h.topics.DisplayModeState(), "off")
	h.waitLast(h.topics.DisplayState(), "OFF")
	h.mq.Deliver(h.topics.DisplayModeSet(), "suspend", false)
	h.waitLast(h.topics.DisplayModeState(), "suspend")
	raw, _ = h.mq.Last(h.topics.DisplayAttributes())
	if !strings.Contains(raw, `"power_save_mode":2`) || !strings.Contains(raw, `"off_mode":"off"`) {
		t.Errorf("display attributes = %s", raw)
	}
}

func TestTemplateSelectStopsBeforeStart(t *testing.T) {
	h := start(t, harnessOpts{seed: func(m *systemd.FakeManager) {
		m.SetStatus(grafana, "active", "running", "loaded")
		m.SetStatus(cam1, "active", "running", "loaded")
		m.SetStatus(cam2, "inactive", "dead", "loaded")
	}})
	h.waitOnline()
	h.waitLast(h.topics.TemplateState("vlc"), "Front door")
	m := h.manager()

	h.mq.Deliver(h.topics.TemplateSet("vlc"), "Parking", false)
	h.waitLast(h.topics.TemplateState("vlc"), "Parking")
	calls := m.Calls()
	if len(calls) != 2 || calls[0] != "stop "+cam1+" replace" || calls[1] != "start "+cam2+" replace" {
		t.Fatalf("calls = %v, want stop cam1 then start cam2", calls)
	}
	h.waitLast(h.topics.UnitState("vlc_cam1"), "OFF")
	h.waitLast(h.topics.UnitState("vlc_cam2"), "ON")

	h.mq.Deliver(h.topics.TemplateSet("vlc"), "off", false)
	h.waitLast(h.topics.TemplateState("vlc"), "off")
	calls = m.Calls()
	if len(calls) != 4 || calls[2] != "stop "+cam1+" replace" || calls[3] != "stop "+cam2+" replace" {
		t.Fatalf("calls after off = %v", calls)
	}

	h.mq.Deliver(h.topics.TemplateSet("vlc"), "Nope", false)
	time.Sleep(5 * reconcileDelay)
	if len(m.Calls()) != 4 {
		t.Errorf("unknown label ran jobs: %v", m.Calls())
	}

	// Starting an instance by hand flips the select without any template command.
	m.SetStatus(cam1, "active", "running", "loaded")
	m.TriggerWake()
	h.waitLast(h.topics.TemplateState("vlc"), "Front door")
}

func TestPublishCacheDedupesButRepublishForces(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()
	m := h.manager()
	topic := h.topics.UnitState("grafana_kiosk")

	// A real change goes out once.
	m.SetStatus(grafana, "inactive", "dead", "loaded")
	m.TriggerWake()
	h.waitLast(topic, "OFF")
	before := h.mq.Count(topic)

	// Wakes without a change publish nothing.
	m.TriggerWake()
	time.Sleep(5 * reconcileDelay)
	m.TriggerWake()
	time.Sleep(5 * reconcileDelay)
	if n := h.mq.Count(topic); n != before {
		t.Fatalf("unchanged state republished: %d -> %d", before, n)
	}
	attrsBefore := h.mq.Count(h.topics.UnitAttributes("grafana_kiosk"))

	h.app.RepublishAll()
	if n := h.mq.Count(topic); n != before+1 {
		t.Fatalf("RepublishAll did not force the state topic: %d -> %d", before, n)
	}
	if n := h.mq.Count(h.topics.UnitAttributes("grafana_kiosk")); n != attrsBefore+1 {
		t.Fatalf("RepublishAll did not force attributes: %d -> %d", attrsBefore, n)
	}
	pubs := h.mq.Publishes()
	if last := pubs[len(pubs)-1]; last.topic != h.topics.Availability() || last.payload != "online" {
		t.Errorf("RepublishAll should end with availability online, got %+v", last)
	}
}

func TestSystemScopeRedialAndPolkitDenied(t *testing.T) {
	denied := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"Interactive authentication required."}}
	h := start(t, harnessOpts{yaml: systemYAML, noDisplay: true, seed: func(m *systemd.FakeManager) {
		if m.Scope() == config.ScopeSystem {
			m.SetStatus("cron.service", "active", "running", "loaded")
			m.SetJobResult("cron.service", systemd.JobError, denied)
		} else {
			m.SetStatus(grafana, "active", "running", "loaded")
		}
	}})
	h.waitOnline()
	sysAvail := h.topics.BusAvailability(config.ScopeSystem)
	h.waitLast(sysAvail, "online")
	raw, _ := h.mq.Last(h.topics.DaemonState())
	if !strings.Contains(raw, `"bus_system_online":true`) {
		t.Errorf("daemon state should report the system bus: %s", raw)
	}

	sys := h.managerFor(config.ScopeSystem)

	// Polkit denial lands in the attributes and does not touch the switch.
	h.mq.Deliver(h.topics.UnitSet("cron"), "OFF", false)
	h.waitFor("denied job in attributes", func() bool {
		raw, _ := h.mq.Last(h.topics.UnitAttributes("cron"))
		return strings.Contains(raw, `"last_job_result":"error"`)
	})
	attrs := h.attributes("cron")
	if msg, _ := attrs["last_job_error"].(string); !strings.Contains(msg, "Interactive authentication required") {
		t.Errorf("last_job_error = %v", attrs["last_job_error"])
	}
	if got, _ := h.mq.Last(h.topics.UnitState("cron")); got != "ON" {
		t.Errorf("cron state = %q, want ON (a denied stop changes nothing)", got)
	}

	// Two failed queries in a row make the supervisor redial: offline, then online again.
	dials := h.dialer.Dials()
	sys.FailNextQuery(errors.New("boom"))
	sys.FailNextQuery(errors.New("boom"))
	sys.TriggerWake()
	time.Sleep(5 * reconcileDelay)
	sys.TriggerWake()
	h.waitFor("redial", func() bool { return h.dialer.Dials() > dials })
	h.waitFor("system bus offline then online", func() bool {
		ps := h.mq.PublishesTo(sysAvail)
		return len(ps) >= 3 && ps[len(ps)-2] == "offline" && ps[len(ps)-1] == "online"
	})
	if !sys.Closed() {
		t.Errorf("old system manager was not closed after redial")
	}
}

func TestDaemonRestartStopsRun(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()
	h.mq.Deliver(h.topics.DaemonRestart(), "PRESS", false)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after a restart command")
	}
	if topic, n := h.mq.ClosedWith(); topic != h.topics.Availability() || n != 1 {
		t.Errorf("Close called %d times with %q", n, topic)
	}
	if m := h.manager(); !m.Closed() {
		t.Errorf("manager not closed on shutdown")
	}
	if !h.disp.Closed() {
		t.Errorf("display not closed on shutdown")
	}
	// Cleanup expects one Run result; put it back.
	h.done <- nil
}

func TestMQTTReconnectRepublishes(t *testing.T) {
	h := start(t, harnessOpts{seed: seedKiosk})
	h.waitOnline()
	h.mq.Reconnect(errors.New("broker went away"))
	h.waitFor("republish after reconnect", func() bool { return h.mq.Count(h.topics.DiscoveryConfig()) == 2 })
	h.waitFor("reconnect counter", func() bool {
		raw, _ := h.mq.Last(h.topics.DaemonState())
		return strings.Contains(raw, `"mqtt_reconnects":1`)
	})
}
