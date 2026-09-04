package app

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/ha"
)

// publish sends one topic through the last-payload cache. Retained topics are
// skipped when the payload did not change, unless a republish is forcing them
// out. It never blocks: the MQTT client queues internally.
func (a *App) publish(topic string, payload []byte, retained bool) {
	if a.mq == nil {
		return
	}
	if retained && !a.force {
		if prev, ok := a.last[topic]; ok && bytes.Equal(prev, payload) {
			return
		}
	}
	if retained {
		a.last[topic] = payload
	}
	a.mq.Publish(topic, payload, retained)
}

func (a *App) publishString(topic, payload string) {
	a.publish(topic, []byte(payload), true)
}

// republishAll is the ordered full publish: discovery first so HA knows the
// entities, every cached state next, and availability online last so nothing
// is ever available with stale state behind it. It then schedules a fresh
// reconcile of every scope and a display read, since the cache may be old.
func (a *App) republishAll() {
	a.force = true
	defer func() { a.force = false }()

	count := 0
	if a.cfg.HAEnabled() {
		if a.publishDiscovery() {
			count++
		}
	}
	for _, scope := range a.cfg.Scopes() {
		a.publishBus(scope)
		count++
	}
	for _, snap := range a.st.Units() {
		if snap.Known {
			a.publishUnit(snap)
			count += 2
		}
	}
	for _, sel := range a.cfg.Selects() {
		if a.publishTemplate(sel) {
			count++
		}
	}
	count += a.publishDisplay()
	a.publishDaemonState()
	count++
	a.publishString(a.t.Availability(), "online")
	count++
	a.log.Info("published everything", "topics", count)

	for _, scope := range a.cfg.Scopes() {
		a.scheduleReconcile(scope)
	}
	a.refreshDisplay()
}

func (a *App) publishDiscovery() bool {
	payload, err := ha.Marshal(ha.BuildDiscovery(a.cfg, a.t, a.version()))
	if err != nil {
		a.log.Error("cannot encode discovery payload", "err", err)
		return false
	}
	a.publish(a.t.DiscoveryConfig(), payload, true)
	a.log.Debug("published discovery", "topic", a.t.DiscoveryConfig(), "bytes", len(payload))
	return true
}

func (a *App) publishBus(scope config.Scope) {
	state := "offline"
	if a.busOnline[scope] {
		state = "online"
	}
	a.publishString(a.t.BusAvailability(scope), state)
}

// unitAttributes is the JSON on R/unit/<id>/attributes.
type unitAttributes struct {
	Unit          string `json:"unit"`
	Scope         string `json:"scope"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	LoadState     string `json:"load_state"`
	LastJob       string `json:"last_job,omitempty"`
	LastJobResult string `json:"last_job_result,omitempty"`
	LastJobError  string `json:"last_job_error,omitempty"`
	LastJobAt     string `json:"last_job_at,omitempty"`
	ChangedAt     string `json:"changed_at"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// publishUnit sends the switch state and the attributes of one known unit.
func (a *App) publishUnit(snap UnitSnapshot) {
	if !snap.Known {
		return
	}
	state := "OFF"
	if snap.IsOn() {
		state = "ON"
	}
	a.publishString(a.t.UnitState(snap.ID), state)

	attrs := unitAttributes{
		Unit:          snap.Name,
		Scope:         string(snap.Scope),
		ActiveState:   snap.ActiveState,
		SubState:      snap.SubState,
		LoadState:     snap.LoadState,
		LastJob:       snap.LastJob,
		LastJobResult: snap.LastJobResult,
		LastJobError:  snap.LastJobError,
		LastJobAt:     rfc3339(snap.LastJobAt),
		ChangedAt:     rfc3339(snap.ChangedAt),
	}
	payload, err := json.Marshal(attrs)
	if err != nil {
		a.log.Error("cannot encode unit attributes", "unit", snap.Name, "err", err)
		return
	}
	a.publish(a.t.UnitAttributes(snap.ID), payload, true)
}

// publishTemplate sends the select state of an exclusive template once at
// least one instance has been queried. Returns whether anything was published.
func (a *App) publishTemplate(sel config.TemplateSelect) bool {
	label, active, known := a.st.TemplateState(sel)
	if !known {
		return false
	}
	if len(active) > 1 {
		a.log.Warn("several instances of an exclusive template are active, reporting the first",
			"template", sel.ID, "active", active)
	}
	a.publishString(a.t.TemplateState(sel.ID), label)
	return true
}

// publishDisplay sends the display topics. When Mutter is unreachable or
// reports -1 only the availability topic goes out, so the last real state
// stays retained behind an unavailable entity. Returns the topic count.
func (a *App) publishDisplay() int {
	if !a.cfg.DisplayEnabled() {
		return 0
	}
	if !a.displayAvailable() {
		a.publishString(a.t.DisplayAvailability(), "offline")
		return 1
	}
	a.publishString(a.t.DisplayState(), a.dispMode.SwitchState())
	a.publishString(a.t.DisplayModeState(), a.dispMode.String())
	attrs, err := ha.Marshal(map[string]any{
		"power_save_mode": int(a.dispMode),
		"mode":            a.dispMode.String(),
		"off_mode":        a.cfg.Display.OffMode,
	})
	if err == nil {
		a.publish(a.t.DisplayAttributes(), attrs, true)
	}
	a.publishString(a.t.DisplayAvailability(), "online")
	return 4
}

// publishDaemonState sends the diagnostics JSON. No heartbeat in here on
// purpose: it changes only when something changed.
func (a *App) publishDaemonState() {
	var reconnects int64
	if a.mq != nil {
		reconnects = a.mq.Reconnects()
	}
	st := map[string]any{
		"version":         a.version(),
		"commit":          a.commit(),
		"started_at":      rfc3339(a.startedAt),
		"hostname":        a.hostname,
		"bus_user_online": a.busOnline[config.ScopeUser],
		"display_online":  a.displayAvailable(),
		"mqtt_reconnects": reconnects,
	}
	if _, ok := a.sup[config.ScopeSystem]; ok {
		st["bus_system_online"] = a.busOnline[config.ScopeSystem]
	}
	payload, err := ha.Marshal(st)
	if err != nil {
		a.log.Error("cannot encode daemon state", "err", err)
		return
	}
	a.publish(a.t.DaemonState(), payload, true)
}
