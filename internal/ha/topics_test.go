package ha

import (
	"slices"
	"testing"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

func TestParse(t *testing.T) {
	cfg := loadTestConfig(t, "office-kiosk.yaml")
	tp := NewTopics(cfg)
	cases := []struct {
		topic string
		want  Inbound
		ok    bool
	}{
		{"systemd2mqtt/office-kiosk/unit/vlc_cam1/set", Inbound{Kind: InUnitSet, ID: "vlc_cam1"}, true},
		{"systemd2mqtt/office-kiosk/template/vlc/set", Inbound{Kind: InTemplateSet, ID: "vlc"}, true},
		{"systemd2mqtt/office-kiosk/display/set", Inbound{Kind: InDisplaySet}, true},
		{"systemd2mqtt/office-kiosk/display/mode/set", Inbound{Kind: InDisplayModeSet}, true},
		{"systemd2mqtt/office-kiosk/daemon/restart", Inbound{Kind: InDaemonRestart}, true},
		{"homeassistant/status", Inbound{Kind: InHAStatus}, true},
		{"systemd2mqtt/office-kiosk/unit/vlc_cam1/state", Inbound{}, false},
		{"systemd2mqtt/office-kiosk/unit//set", Inbound{}, false},
		{"systemd2mqtt/office-kiosk/unit/set", Inbound{}, false},
		{"systemd2mqtt/office-kiosk/bogus/x/set", Inbound{}, false},
		{"systemd2mqtt/other-node/unit/vlc_cam1/set", Inbound{}, false},
		{"systemd2mqtt/office-kiosk", Inbound{}, false},
		{"", Inbound{}, false},
		{"junk", Inbound{}, false},
	}
	for _, c := range cases {
		got, ok := tp.Parse(c.topic)
		if ok != c.ok || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", c.topic, got, ok, c.want, c.ok)
		}
	}
}

func TestSubscriptions(t *testing.T) {
	cfg := loadTestConfig(t, "office-kiosk.yaml")
	tp := NewTopics(cfg)
	subs := tp.Subscriptions(cfg)
	for _, want := range []string{
		"systemd2mqtt/office-kiosk/unit/+/set",
		"systemd2mqtt/office-kiosk/template/+/set",
		"systemd2mqtt/office-kiosk/daemon/restart",
		"systemd2mqtt/office-kiosk/display/set",
		"systemd2mqtt/office-kiosk/display/mode/set",
		"homeassistant/status",
	} {
		if !slices.Contains(subs, want) {
			t.Errorf("missing subscription %q in %v", want, subs)
		}
	}
	if len(subs) != 6 {
		t.Errorf("got %d subscriptions: %v", len(subs), subs)
	}
}

// TestRetainedTopicsCoverDiscovery checks that everything the discovery payload
// points HA at (state, attributes, availability) is blanked by --clear-discovery,
// and that the discovery topic itself is first.
func TestRetainedTopicsCoverDiscovery(t *testing.T) {
	for _, yaml := range []string{
		"",
		`
node_id: box
mqtt: { broker: tcp://localhost:1883 }
units:
  - { name: cron.service, scope: system }
templates:
  - { name: vlc@.service, scope: user, exclusive: true, instances: [a, b] }
`,
	} {
		var cfg *config.Config
		if yaml == "" {
			cfg = loadTestConfig(t, "office-kiosk.yaml")
		} else {
			cfg = parseTestConfig(t, yaml)
		}
		tp := NewTopics(cfg)
		retained := tp.RetainedTopics(cfg)
		if retained[0] != tp.DiscoveryConfig() {
			t.Errorf("first retained topic is %q, want the discovery config", retained[0])
		}
		seen := map[string]bool{}
		for _, r := range retained {
			if seen[r] {
				t.Errorf("duplicate retained topic %q", r)
			}
			seen[r] = true
		}
		cmps := BuildDiscovery(cfg, tp, "dev")["cmps"].(map[string]any)
		for id, raw := range cmps {
			comp := raw.(map[string]any)
			for _, key := range []string{"stat_t", "json_attr_t"} {
				if topic, ok := comp[key].(string); ok && !seen[topic] {
					t.Errorf("%s.%s = %q is not in RetainedTopics", id, key, topic)
				}
			}
			if avty, ok := comp["avty"].([]map[string]any); ok {
				for _, a := range avty {
					if topic := a["topic"].(string); !seen[topic] {
						t.Errorf("%s availability %q is not in RetainedTopics", id, topic)
					}
				}
			}
			if topic, ok := comp["cmd_t"].(string); ok && seen[topic] {
				t.Errorf("%s command topic %q must not be retained", id, topic)
			}
		}
		for _, s := range cfg.Scopes() {
			if !seen[tp.BusAvailability(s)] {
				t.Errorf("bus availability for %s missing", s)
			}
		}
	}
}
