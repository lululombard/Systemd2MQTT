package ha

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden discovery file from the current implementation")

// loadTestConfig parses a testdata config with the env overrides cleared so the
// developer's shell cannot leak into the golden output.
func loadTestConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	for _, k := range []string{"SYSTEMD2MQTT_MQTT_BROKER", "SYSTEMD2MQTT_MQTT_USERNAME", "SYSTEMD2MQTT_MQTT_PASSWORD"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return cfg
}

func parseTestConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	for _, k := range []string{"SYSTEMD2MQTT_MQTT_BROKER", "SYSTEMD2MQTT_MQTT_USERNAME", "SYSTEMD2MQTT_MQTT_PASSWORD"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func TestBuildDiscoveryGolden(t *testing.T) {
	cfg := loadTestConfig(t, "office-kiosk.yaml")
	got, err := Marshal(BuildDiscovery(cfg, NewTopics(cfg), "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "office-kiosk.discovery.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("discovery payload differs from %s (run go test ./internal/ha -update after checking the diff)\n got: %s\nwant: %s", golden, got, want)
	}
	if got[len(got)-1] == '\n' {
		t.Error("Marshal must not end with a newline")
	}

	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("golden is not valid JSON: %v", err)
	}
	cmps := back["cmps"].(map[string]any)
	if len(cmps) != 19 {
		t.Errorf("example config must give 19 components, got %d", len(cmps))
	}
	dev := back["dev"].(map[string]any)
	if ids := dev["ids"].([]any); len(ids) != 1 || ids[0] != "systemd2mqtt_office-kiosk" {
		t.Errorf("dev.ids = %v", dev["ids"])
	}
	if dev["sa"] != "Office" || dev["name"] != "Office kiosk" || dev["sw"] != "1.0.0" {
		t.Errorf("dev = %v", dev)
	}
	sel := cmps["template_vlc"].(map[string]any)
	wantOps := []any{"off", "Front door", "Parking"}
	if ops := sel["ops"].([]any); len(ops) != len(wantOps) || ops[0] != wantOps[0] || ops[1] != wantOps[1] || ops[2] != wantOps[2] {
		t.Errorf("template_vlc ops = %v, want %v", ops, wantOps)
	}
	if _, ok := cmps["bus_system"]; ok {
		t.Error("bus_system must not exist without a system-scope unit")
	}
}

func TestBuildDiscoveryComponentsHaveIdentity(t *testing.T) {
	// A second config with a system unit, opt-outs and no display, so every branch runs.
	cfg := parseTestConfig(t, `
node_id: box
mqtt: { broker: tcp://localhost:1883, qos: 2 }
units:
  - { name: grafana-kiosk.service, scope: user }
  - { name: cron.service, scope: system, restart_button: false, problem_sensor: false }
templates:
  - name: vlc@.service
    scope: system
    exclusive: false
    instances: [cam1]
display: { enabled: false }
`)
	for _, c := range []struct {
		name string
		cfg  *config.Config
	}{
		{"office-kiosk", loadTestConfig(t, "office-kiosk.yaml")},
		{"mixed", cfg},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := BuildDiscovery(c.cfg, NewTopics(c.cfg), "dev")
			cmps := payload["cmps"].(map[string]any)
			seen := map[string]string{}
			for id, raw := range cmps {
				comp := raw.(map[string]any)
				if p, _ := comp["p"].(string); p == "" {
					t.Errorf("%s: missing p", id)
				}
				uid, _ := comp["uniq_id"].(string)
				if uid == "" {
					t.Errorf("%s: missing uniq_id", id)
				}
				if prev, dup := seen[uid]; dup {
					t.Errorf("uniq_id %q used by %s and %s", uid, prev, id)
				}
				seen[uid] = id
				if want := DeviceID(c.cfg.NodeID) + "_" + id; uid != want {
					t.Errorf("%s: uniq_id %q, want %q", id, uid, want)
				}
			}
		})
	}

	cmps := BuildDiscovery(cfg, NewTopics(cfg), "dev")["cmps"].(map[string]any)
	for _, absent := range []string{"unit_cron_restart", "unit_cron_problem", "display", "display_mode", "template_vlc"} {
		if _, ok := cmps[absent]; ok {
			t.Errorf("%s should not be present", absent)
		}
	}
	for _, present := range []string{"unit_cron", "unit_cron_state", "unit_vlc_cam1", "unit_vlc_cam1_restart", "bus_user", "bus_system", "version", "started", "daemon_restart"} {
		if _, ok := cmps[present]; !ok {
			t.Errorf("%s should be present", present)
		}
	}
	if len(cmps) != 4+2+4+2+3 {
		t.Errorf("got %d components", len(cmps))
	}
	root := BuildDiscovery(cfg, NewTopics(cfg), "dev")
	if root["qos"] != 2 {
		t.Errorf("qos = %v", root["qos"])
	}
	if _, ok := root["dev"].(map[string]any)["sa"]; ok {
		t.Error("sa must be omitted when suggested_area is empty")
	}
}

func TestMarshalNoHTMLEscape(t *testing.T) {
	out, err := Marshal(map[string]any{"val_tpl": "{{ a < b and c > d }}", "b": 1, "a": "&"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"&","b":1,"val_tpl":"{{ a < b and c > d }}"}`
	if string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}
