package ha

import (
	"bytes"
	"encoding/json"

	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
)

// ProjectURL is what Home Assistant shows as the origin link.
const ProjectURL = "https://github.com/lululombard/Systemd2MQTT"

// DeviceID is the discovery device identifier for a node.
func DeviceID(nodeID string) string { return "systemd2mqtt_" + nodeID }

// BuildDiscovery renders the device-based discovery payload (HA 2024.11+).
// Keys are the abbreviated ones so the retained payload stays small. Every
// component carries its own availability list so a dead bus or an unknown
// display state only greys out the entities that depend on it.
func BuildDiscovery(cfg *config.Config, t Topics, version string) map[string]any {
	devID := DeviceID(cfg.NodeID)
	avail := t.Availability()

	dev := map[string]any{
		"ids":  []string{devID},
		"name": cfg.Device.Name,
		"mf":   cfg.Device.Manufacturer,
		"mdl":  cfg.Device.Model,
		"sw":   version,
	}
	if cfg.Device.SuggestedArea != "" {
		dev["sa"] = cfg.Device.SuggestedArea
	}

	cmps := map[string]any{}
	add := func(id string, c map[string]any) {
		c["uniq_id"] = devID + "_" + id
		cmps[id] = c
	}
	avty := func(topics ...string) []map[string]any {
		out := make([]map[string]any, 0, len(topics))
		for _, tp := range topics {
			out = append(out, map[string]any{"topic": tp})
		}
		return out
	}

	for _, u := range cfg.AllUnits() {
		id := u.ObjectID()
		unitAvty := avty(avail, t.BusAvailability(u.Scope))
		add("unit_"+id, map[string]any{
			"p":           "switch",
			"name":        u.DisplayName(),
			"ic":          u.Icon,
			"stat_t":      t.UnitState(id),
			"cmd_t":       t.UnitSet(id),
			"pl_on":       "ON",
			"pl_off":      "OFF",
			"json_attr_t": t.UnitAttributes(id),
			"avty":        unitAvty,
			"avty_mode":   "all",
		})
		add("unit_"+id+"_state", map[string]any{
			"p":           "sensor",
			"name":        u.DisplayName() + " state",
			"ent_cat":     "diagnostic",
			"ic":          "mdi:state-machine",
			"stat_t":      t.UnitAttributes(id),
			"val_tpl":     "{{ value_json.active_state }} ({{ value_json.sub_state }})",
			"json_attr_t": t.UnitAttributes(id),
			"avty":        unitAvty,
			"avty_mode":   "all",
		})
		if u.WantsProblemSensor() {
			add("unit_"+id+"_problem", map[string]any{
				"p":         "binary_sensor",
				"name":      u.DisplayName() + " problem",
				"dev_cla":   "problem",
				"ent_cat":   "diagnostic",
				"stat_t":    t.UnitAttributes(id),
				"val_tpl":   "{{ 'ON' if value_json.active_state == 'failed' or value_json.load_state == 'not-found' else 'OFF' }}",
				"avty":      unitAvty,
				"avty_mode": "all",
			})
		}
		if u.WantsRestartButton() {
			add("unit_"+id+"_restart", map[string]any{
				"p":         "button",
				"name":      "Restart " + u.DisplayName(),
				"dev_cla":   "restart",
				"cmd_t":     t.UnitSet(id),
				"pl_prs":    "RESTART",
				"avty":      unitAvty,
				"avty_mode": "all",
			})
		}
	}

	for _, s := range cfg.Selects() {
		ops := []string{"off"}
		for _, o := range s.Options {
			ops = append(ops, o.Label)
		}
		add("template_"+s.ID, map[string]any{
			"p":         "select",
			"name":      s.Name,
			"ic":        s.Icon,
			"ops":       ops,
			"stat_t":    t.TemplateState(s.ID),
			"cmd_t":     t.TemplateSet(s.ID),
			"avty":      avty(avail, t.BusAvailability(s.Scope)),
			"avty_mode": "all",
		})
	}

	if cfg.DisplayEnabled() {
		displayAvty := avty(avail, t.DisplayAvailability())
		add("display", map[string]any{
			"p":           "switch",
			"name":        "Display",
			"ic":          "mdi:monitor",
			"dev_cla":     "switch",
			"stat_t":      t.DisplayState(),
			"cmd_t":       t.DisplaySet(),
			"pl_on":       "ON",
			"pl_off":      "OFF",
			"json_attr_t": t.DisplayAttributes(),
			"avty":        displayAvty,
			"avty_mode":   "all",
		})
		if cfg.ExposeModeSelect() {
			add("display_mode", map[string]any{
				"p":         "select",
				"name":      "Display power mode",
				"ent_cat":   "config",
				"ic":        "mdi:monitor-shimmer",
				"ops":       append([]string(nil), display.SelectOptions...),
				"stat_t":    t.DisplayModeState(),
				"cmd_t":     t.DisplayModeSet(),
				"avty":      displayAvty,
				"avty_mode": "all",
			})
		}
	}

	for _, scope := range cfg.Scopes() {
		name := "System bus"
		if scope == config.ScopeUser {
			name = "Session bus"
		}
		add("bus_"+string(scope), map[string]any{
			"p":       "binary_sensor",
			"name":    name,
			"dev_cla": "connectivity",
			"ent_cat": "diagnostic",
			"stat_t":  t.BusAvailability(scope),
			"pl_on":   "online",
			"pl_off":  "offline",
		})
	}
	add("version", map[string]any{
		"p":           "sensor",
		"name":        "Version",
		"ent_cat":     "diagnostic",
		"ic":          "mdi:tag",
		"stat_t":      t.DaemonState(),
		"val_tpl":     "{{ value_json.version }}",
		"json_attr_t": t.DaemonState(),
	})
	add("started", map[string]any{
		"p":       "sensor",
		"name":    "Started",
		"ent_cat": "diagnostic",
		"dev_cla": "timestamp",
		"stat_t":  t.DaemonState(),
		"val_tpl": "{{ value_json.started_at }}",
	})
	add("daemon_restart", map[string]any{
		"p":       "button",
		"name":    "Restart Systemd2MQTT",
		"dev_cla": "restart",
		"ent_cat": "diagnostic",
		"cmd_t":   t.DaemonRestart(),
		"pl_prs":  "PRESS",
	})

	return map[string]any{
		"dev": dev,
		"o": map[string]any{
			"name": "Systemd2MQTT",
			"sw":   version,
			"url":  ProjectURL,
		},
		"avty":      avty(avail),
		"avty_mode": "all",
		"qos":       int(cfg.MQTT.QoS),
		"cmps":      cmps,
	}
}

// Marshal encodes a discovery payload with sorted keys, no HTML escaping and no
// trailing newline, so the same config always gives the same retained bytes.
func Marshal(m map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
