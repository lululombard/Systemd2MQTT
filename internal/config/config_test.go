package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv makes sure the env overrides do not leak into tests from the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SYSTEMD2MQTT_MQTT_BROKER", "SYSTEMD2MQTT_MQTT_USERNAME", "SYSTEMD2MQTT_MQTT_PASSWORD", "SYSTEMD2MQTT_CONFIG"} {
		t.Setenv(k, "")
	}
}

func load(t *testing.T, name string) *Config {
	t.Helper()
	c, err := Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return c
}

func mustFail(t *testing.T, yaml, want string) {
	t.Helper()
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	c := load(t, "minimal.yaml")

	host, _ := os.Hostname()
	if want := sanitizeNodeID(host); c.NodeID != want {
		t.Errorf("node_id = %q, want %q", c.NodeID, want)
	}
	if !nodeIDRe.MatchString(c.NodeID) {
		t.Errorf("node_id %q does not match %s", c.NodeID, nodeIDRe)
	}
	if c.Device.Name != c.NodeID {
		t.Errorf("device.name = %q, want node_id %q", c.Device.Name, c.NodeID)
	}
	if c.Device.Manufacturer != "lululombard" || c.Device.Model != "Systemd2MQTT" {
		t.Errorf("device = %+v", c.Device)
	}
	if c.MQTT.Broker != "tcp://10.2.0.1:1883" {
		t.Errorf("mqtt.broker = %q", c.MQTT.Broker)
	}
	if want := "systemd2mqtt-" + c.NodeID; c.MQTT.ClientID != want {
		t.Errorf("mqtt.client_id = %q, want %q", c.MQTT.ClientID, want)
	}
	if c.MQTT.BaseTopic != "systemd2mqtt" {
		t.Errorf("mqtt.base_topic = %q", c.MQTT.BaseTopic)
	}
	if c.MQTT.KeepAlive != 30*time.Second {
		t.Errorf("mqtt.keepalive = %s", c.MQTT.KeepAlive)
	}
	if c.MQTT.UsesTLS() {
		t.Error("tcp:// must not use TLS")
	}
	if !c.HAEnabled() || c.HomeAssistant.Enabled == nil || !*c.HomeAssistant.Enabled {
		t.Error("homeassistant.enabled should default to true")
	}
	if c.HomeAssistant.DiscoveryPrefix != "homeassistant" {
		t.Errorf("discovery_prefix = %q", c.HomeAssistant.DiscoveryPrefix)
	}
	if c.HomeAssistant.StatusTopic != "homeassistant/status" {
		t.Errorf("status_topic = %q", c.HomeAssistant.StatusTopic)
	}

	units := c.AllUnits()
	if len(units) != 2 {
		t.Fatalf("AllUnits = %+v, want 2 default units", units)
	}
	wantNames := []string{"grafana-kiosk.service", "vlc.service"}
	for i, u := range units {
		if u.Name != wantNames[i] {
			t.Errorf("units[%d].name = %q, want %q", i, u.Name, wantNames[i])
		}
		if u.Scope != ScopeUser {
			t.Errorf("units[%d].scope = %q, want user", i, u.Scope)
		}
		if u.Icon != "mdi:cog-play" {
			t.Errorf("units[%d].icon = %q", i, u.Icon)
		}
		if u.TemplateID != "" {
			t.Errorf("units[%d].TemplateID = %q, want empty", i, u.TemplateID)
		}
		if !u.WantsRestartButton() || !u.WantsProblemSensor() {
			t.Errorf("units[%d] should want restart button and problem sensor by default", i)
		}
	}
	if units[0].ObjectID() != "grafana_kiosk" || units[1].ObjectID() != "vlc" {
		t.Errorf("object ids = %q, %q", units[0].ObjectID(), units[1].ObjectID())
	}
	if units[0].DisplayName() != "Grafana-kiosk" || units[1].DisplayName() != "Vlc" {
		t.Errorf("display names = %q, %q", units[0].DisplayName(), units[1].DisplayName())
	}
	if got := c.Scopes(); len(got) != 1 || got[0] != ScopeUser {
		t.Errorf("Scopes = %v, want [user]", got)
	}
	if len(c.UnitsForScope(ScopeSystem)) != 0 {
		t.Error("no system units expected")
	}
	if u, ok := c.UnitByID("vlc"); !ok || u.Name != "vlc.service" {
		t.Errorf("UnitByID(vlc) = %+v, %v", u, ok)
	}
	if _, ok := c.UnitByID("nope"); ok {
		t.Error("UnitByID(nope) should not be found")
	}
	if len(c.Selects()) != 0 {
		t.Errorf("Selects = %+v, want none", c.Selects())
	}

	if !c.DisplayEnabled() {
		t.Error("display.enabled should default to true")
	}
	if c.Display.OffMode != "off" {
		t.Errorf("display.off_mode = %q", c.Display.OffMode)
	}
	if !c.ExposeModeSelect() {
		t.Error("display.expose_mode_select should default to true")
	}
	if c.Systemd.PollInterval != 30*time.Second {
		t.Errorf("systemd.poll_interval = %s", c.Systemd.PollInterval)
	}
	if c.Systemd.JobTimeout != 60*time.Second {
		t.Errorf("systemd.job_timeout = %s", c.Systemd.JobTimeout)
	}
	if c.Systemd.JobMode != "replace" {
		t.Errorf("systemd.job_mode = %q", c.Systemd.JobMode)
	}
	if c.LogLevel != "info" {
		t.Errorf("log_level = %q", c.LogLevel)
	}
}

func TestFull(t *testing.T) {
	clearEnv(t)
	c := load(t, "full.yaml")

	if c.NodeID != "office-kiosk" {
		t.Errorf("node_id = %q", c.NodeID)
	}
	if c.Device.Name != "Office kiosk" || c.Device.Manufacturer != "Lucas" || c.Device.Model != "Kiosk" || c.Device.SuggestedArea != "Office" {
		t.Errorf("device = %+v", c.Device)
	}
	if c.MQTT.ClientID != "kiosk-mqtt" || c.MQTT.Username != "systemd2mqtt" || c.MQTT.Password != "change-me" {
		t.Errorf("mqtt = %+v", c.MQTT)
	}
	if c.MQTT.BaseTopic != "kiosk" || c.MQTT.KeepAlive != 45*time.Second || c.MQTT.QoS != 1 {
		t.Errorf("mqtt = %+v", c.MQTT)
	}
	if c.HAEnabled() {
		t.Error("homeassistant.enabled: false should stick")
	}
	if c.HomeAssistant.DiscoveryPrefix != "ha" || c.HomeAssistant.StatusTopic != "ha/status" {
		t.Errorf("homeassistant = %+v", c.HomeAssistant)
	}

	units := c.AllUnits()
	if len(units) != 2 {
		t.Fatalf("AllUnits = %+v", units)
	}
	g := units[0]
	if g.Name != "grafana-kiosk.service" || g.Scope != ScopeUser || g.ObjectID() != "grafana_kiosk" {
		t.Errorf("units[0] = %+v", g)
	}
	if g.DisplayName() != "Grafana kiosk" || g.Icon != "mdi:monitor-dashboard" {
		t.Errorf("units[0] = %+v", g)
	}
	nm := units[1]
	if nm.Name != "NetworkManager.service" || nm.Scope != ScopeSystem || nm.ObjectID() != "nm" {
		t.Errorf("units[1] = %+v", nm)
	}
	if nm.WantsRestartButton() || nm.WantsProblemSensor() {
		t.Errorf("units[1] should have restart button and problem sensor turned off: %+v", nm)
	}
	if nm.DisplayName() != "NetworkManager" {
		t.Errorf("units[1].DisplayName = %q", nm.DisplayName())
	}
	if got := c.Scopes(); len(got) != 2 || got[0] != ScopeUser || got[1] != ScopeSystem {
		t.Errorf("Scopes = %v, want [user system]", got)
	}
	if got := c.UnitsForScope(ScopeSystem); len(got) != 1 || got[0].Name != "NetworkManager.service" {
		t.Errorf("UnitsForScope(system) = %+v", got)
	}
	if u, ok := c.UnitByID("nm"); !ok || u.Name != "NetworkManager.service" {
		t.Errorf("UnitByID(nm) = %+v, %v", u, ok)
	}

	if c.DisplayEnabled() || c.ExposeModeSelect() {
		t.Errorf("display = %+v", c.Display)
	}
	if c.Display.OffMode != "suspend" || c.Display.OffModeValue() != 2 {
		t.Errorf("display = %+v", c.Display)
	}
	if c.Systemd.PollInterval != 10*time.Second || c.Systemd.JobTimeout != 20*time.Second || c.Systemd.JobMode != "fail" {
		t.Errorf("systemd = %+v", c.Systemd)
	}
	if c.LogLevel != "debug" {
		t.Errorf("log_level = %q", c.LogLevel)
	}
}

func TestExplicitEmptyUnits(t *testing.T) {
	clearEnv(t)
	c, err := Parse([]byte("mqtt:\n  broker: tcp://broker:1883\nunits: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllUnits()) != 0 {
		t.Errorf("AllUnits = %+v, want none", c.AllUnits())
	}
	if len(c.Scopes()) != 0 {
		t.Errorf("Scopes = %v, want none", c.Scopes())
	}
}

func TestTemplatesOnlyDisablesDefaultUnits(t *testing.T) {
	clearEnv(t)
	c, err := Parse([]byte(`
mqtt:
  broker: tcp://broker:1883
templates:
  - name: vlc@.service
    scope: user
    instances: [cam1]
`))
	if err != nil {
		t.Fatal(err)
	}
	units := c.AllUnits()
	if len(units) != 1 || units[0].Name != "vlc@cam1.service" {
		t.Errorf("AllUnits = %+v, want only the template instance", units)
	}
	// No friendly_name on the template: it is derived from the unit name.
	if units[0].DisplayName() != "Vlc cam1" {
		t.Errorf("DisplayName = %q", units[0].DisplayName())
	}
	if units[0].TemplateID != "vlc" {
		t.Errorf("TemplateID = %q", units[0].TemplateID)
	}
	if len(c.Selects()) != 0 {
		t.Errorf("non exclusive template must not register a select: %+v", c.Selects())
	}
}

func TestTemplates(t *testing.T) {
	clearEnv(t)
	c := load(t, "templates.yaml")

	units := c.AllUnits()
	if len(units) != 3 {
		t.Fatalf("AllUnits = %+v, want grafana + 2 instances", units)
	}
	if units[0].Name != "grafana-kiosk.service" || units[0].TemplateID != "" {
		t.Errorf("units[0] = %+v", units[0])
	}

	type want struct {
		name, id, friendly string
	}
	wants := []want{
		{"vlc@cam1.service", "vlc_cam1", "Camera cam1"},
		{"vlc@cam2.service", "vlc_cam2", "Parking"},
	}
	for i, w := range wants {
		u := units[i+1]
		if u.Name != w.name {
			t.Errorf("instance %d name = %q, want %q", i, u.Name, w.name)
		}
		if u.ObjectID() != w.id || u.ID != w.id {
			t.Errorf("instance %d id = %q/%q, want %q", i, u.ID, u.ObjectID(), w.id)
		}
		if u.DisplayName() != w.friendly {
			t.Errorf("instance %d friendly name = %q, want %q", i, u.DisplayName(), w.friendly)
		}
		if u.Scope != ScopeUser {
			t.Errorf("instance %d scope = %q", i, u.Scope)
		}
		if u.Icon != "mdi:cctv" {
			t.Errorf("instance %d icon = %q", i, u.Icon)
		}
		if u.TemplateID != "vlc" {
			t.Errorf("instance %d TemplateID = %q, want vlc", i, u.TemplateID)
		}
	}

	sels := c.Selects()
	if len(sels) != 1 {
		t.Fatalf("Selects = %+v, want one", sels)
	}
	s := sels[0]
	if s.ID != "vlc" || s.Name != "Camera" || s.Icon != "mdi:cctv" || s.Scope != ScopeUser {
		t.Errorf("select = %+v", s)
	}
	if len(s.Options) != 2 {
		t.Fatalf("select options = %+v", s.Options)
	}
	wantOpts := []SelectOption{
		{Label: "Camera cam1", UnitID: "vlc_cam1", UnitName: "vlc@cam1.service"},
		{Label: "Parking", UnitID: "vlc_cam2", UnitName: "vlc@cam2.service"},
	}
	for i, o := range s.Options {
		if o != wantOpts[i] {
			t.Errorf("option %d = %+v, want %+v", i, o, wantOpts[i])
		}
	}
	if u, ok := c.UnitByID("vlc_cam2"); !ok || u.Name != "vlc@cam2.service" {
		t.Errorf("UnitByID(vlc_cam2) = %+v, %v", u, ok)
	}
}

func TestErrors(t *testing.T) {
	clearEnv(t)
	broker := "mqtt:\n  broker: tcp://broker:1883\n"

	t.Run("unknown key", func(t *testing.T) {
		mustFail(t, broker+"bogus: 1\n", "bogus")
	})
	t.Run("unknown nested key", func(t *testing.T) {
		mustFail(t, broker+"display:\n  brightness: 3\n", "brightness")
	})
	t.Run("invalid scope", func(t *testing.T) {
		_, err := Load(filepath.Join("testdata", "invalid-scope.yaml"))
		if err == nil || !strings.Contains(err.Error(), `scope "global" must be user or system`) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing scope", func(t *testing.T) {
		mustFail(t, broker+"units:\n  - name: foo.service\n", "must be user or system")
	})
	t.Run("missing broker", func(t *testing.T) {
		mustFail(t, "log_level: info\n", "mqtt.broker is required")
	})
	t.Run("bad broker scheme", func(t *testing.T) {
		mustFail(t, "mqtt:\n  broker: http://broker:1883\n", "must be one of tcp, mqtt, ssl, tls, mqtts, ws, wss")
	})
	t.Run("duplicate id", func(t *testing.T) {
		mustFail(t, broker+`
units:
  - { name: foo.service, scope: user, id: same }
  - { name: bar.service, scope: user, id: same }
`, `id "same" is already used by foo.service`)
	})
	t.Run("duplicate id via template", func(t *testing.T) {
		mustFail(t, broker+`
units:
  - { name: vlc@cam1.service, scope: user }
templates:
  - name: vlc@.service
    scope: user
    instances: [cam1]
`, `id "vlc_cam1" is already used`)
	})
	t.Run("template name in units", func(t *testing.T) {
		mustFail(t, broker+"units:\n  - { name: vlc@.service, scope: user }\n", `"vlc@.service" is a template unit name`)
	})
	t.Run("duplicate template id", func(t *testing.T) {
		mustFail(t, broker+`
templates:
  - name: vlc@.service
    scope: user
    instances: [cam1]
  - name: mpv@.service
    scope: user
    id: vlc
    instances: [cam2]
`, `id "vlc" is already used by templates[0]`)
	})
	t.Run("same unit twice in one scope", func(t *testing.T) {
		mustFail(t, broker+`
units:
  - { name: foo.service, scope: user, id: a }
  - { name: foo.service, scope: user, id: b }
`, `unit "foo.service" in scope user is already configured as id "a"`)
	})
	t.Run("same unit in two scopes is fine", func(t *testing.T) {
		c, err := Parse([]byte(broker + `
units:
  - { name: foo.service, scope: user, id: a }
  - { name: foo.service, scope: system, id: b }
`))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.AllUnits()) != 2 {
			t.Errorf("AllUnits = %+v", c.AllUnits())
		}
	})
	t.Run("component id collision", func(t *testing.T) {
		mustFail(t, broker+`
units:
  - { name: foo.service, scope: user }
  - { name: foo-state.service, scope: user }
`, `id "foo_state" collides with id "foo" on Home Assistant component "unit_foo_state"`)
	})
	t.Run("component id collision needs the component", func(t *testing.T) {
		// With the state sensor's sibling disabled there is nothing to collide with.
		c, err := Parse([]byte(broker + `
units:
  - { name: foo.service, scope: user, restart_button: false }
  - { name: foo-restart.service, scope: user }
`))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.AllUnits()) != 2 {
			t.Errorf("AllUnits = %+v", c.AllUnits())
		}
	})
	t.Run("template without instances", func(t *testing.T) {
		mustFail(t, broker+"templates:\n  - name: vlc@.service\n    scope: user\n", "instances must not be empty")
	})
	t.Run("bad template name", func(t *testing.T) {
		mustFail(t, broker+"templates:\n  - name: vlc.service\n    scope: user\n    instances: [cam1]\n", "is not a template unit name")
	})
	t.Run("bad instance name", func(t *testing.T) {
		mustFail(t, broker+"templates:\n  - name: vlc@.service\n    scope: user\n    instances: [\"cam 1\"]\n", "bad instance name")
	})
	t.Run("bad unit name", func(t *testing.T) {
		mustFail(t, broker+"units:\n  - { name: grafana, scope: user }\n", "must be a full unit name")
	})
	t.Run("bad off_mode", func(t *testing.T) {
		mustFail(t, broker+"display:\n  off_mode: dpms\n", "display.off_mode")
	})
	t.Run("bad job_mode", func(t *testing.T) {
		mustFail(t, broker+"systemd:\n  job_mode: isolate\n", "systemd.job_mode")
	})
	t.Run("poll too short", func(t *testing.T) {
		mustFail(t, broker+"systemd:\n  poll_interval: 1s\n", "at least 5s")
	})
	t.Run("bad log_level", func(t *testing.T) {
		mustFail(t, broker+"log_level: trace\n", "log_level")
	})
	t.Run("bad qos", func(t *testing.T) {
		mustFail(t, broker+"  qos: 3\n", "mqtt.qos")
	})
	t.Run("wildcard base_topic", func(t *testing.T) {
		mustFail(t, broker+"  base_topic: a/#\n", "mqtt.base_topic")
	})
	t.Run("bad node_id", func(t *testing.T) {
		mustFail(t, broker+"node_id: Office Kiosk\n", "node_id")
	})
	t.Run("empty input", func(t *testing.T) {
		mustFail(t, "", "config file is empty")
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join("testdata", "does-not-exist.yaml")); err == nil {
			t.Fatal("expected an error for a missing file")
		}
	})
}

func TestTLS(t *testing.T) {
	clearEnv(t)
	c := load(t, "tls.yaml")
	if !c.MQTT.UsesTLS() {
		t.Error("ssl:// broker should use TLS")
	}
	if c.MQTT.TLS.CAFile != "/etc/ssl/certs/ca.pem" || c.MQTT.TLS.CertFile != "/etc/ssl/certs/client.pem" || c.MQTT.TLS.KeyFile != "/etc/ssl/private/client.key" {
		t.Errorf("tls = %+v", c.MQTT.TLS)
	}
	if c.MQTT.TLS.InsecureSkipVerify {
		t.Error("insecure_skip_verify should default to false")
	}

	for _, tc := range []struct {
		scheme string
		tls    bool
	}{
		{"ssl", true}, {"tls", true}, {"mqtts", true}, {"wss", true},
		{"tcp", false}, {"mqtt", false}, {"ws", false},
	} {
		m := MQTTConfig{Broker: tc.scheme + "://broker:1883"}
		if got := m.UsesTLS(); got != tc.tls {
			t.Errorf("UsesTLS(%s://) = %v, want %v", tc.scheme, got, tc.tls)
		}
	}
	if (MQTTConfig{Broker: "::not a url"}).UsesTLS() {
		t.Error("unparseable broker must not report TLS")
	}

	mustFail(t, "mqtt:\n  broker: ssl://broker:8883\n  tls:\n    cert_file: /c.pem\n", "must be set together")
	mustFail(t, "mqtt:\n  broker: ssl://broker:8883\n  tls:\n    key_file: /c.key\n", "must be set together")
}

func TestSlugify(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"grafana-kiosk.service", "grafana_kiosk"},
		{"vlc@cam1.service", "vlc_cam1"},
		{"Weird Name.service", "weird_name"},
		{"foo.timer", "foo_timer"},
		{"vlc.service", "vlc"},
		{"vlc@.service", "vlc"},
		{"__odd__.service", "odd"},
	} {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeNodeID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"office-kiosk", "office-kiosk"},
		{"Office Kiosk.local", "office-kiosk-local"},
		{"---", "systemd2mqtt"},
		{"", "systemd2mqtt"},
		{strings.Repeat("a", 80), strings.Repeat("a", 64)},
	} {
		if got := sanitizeNodeID(tc.in); got != tc.want {
			t.Errorf("sanitizeNodeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOffModeValue(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int32
	}{
		{"standby", 1}, {"suspend", 2}, {"off", 3}, {"", 3},
	} {
		if got := (DisplayConfig{OffMode: tc.mode}).OffModeValue(); got != tc.want {
			t.Errorf("OffModeValue(%q) = %d, want %d", tc.mode, got, tc.want)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYSTEMD2MQTT_MQTT_BROKER", "ssl://env-broker:8883")
	t.Setenv("SYSTEMD2MQTT_MQTT_USERNAME", "env-user")
	t.Setenv("SYSTEMD2MQTT_MQTT_PASSWORD", "env-pass")

	c := load(t, "full.yaml")
	if c.MQTT.Broker != "ssl://env-broker:8883" {
		t.Errorf("broker = %q", c.MQTT.Broker)
	}
	if c.MQTT.Username != "env-user" || c.MQTT.Password != "env-pass" {
		t.Errorf("mqtt = %+v", c.MQTT)
	}
	if !c.MQTT.UsesTLS() {
		t.Error("env broker with ssl:// should use TLS")
	}

	// The env can also satisfy the required broker when the file has none.
	if _, err := Parse([]byte("log_level: info\n")); err != nil {
		t.Errorf("broker from env should pass validation: %v", err)
	}
}

func TestRedacted(t *testing.T) {
	clearEnv(t)
	c := load(t, "full.yaml")
	out := c.Redacted()
	if strings.Contains(out, "change-me") {
		t.Errorf("Redacted leaked the password:\n%s", out)
	}
	if !strings.Contains(out, "password: '***'") && !strings.Contains(out, `password: "***"`) && !strings.Contains(out, "password: ***") {
		t.Errorf("Redacted should mask the password:\n%s", out)
	}
	if !strings.Contains(out, "node_id: office-kiosk") {
		t.Errorf("Redacted should still contain the rest of the config:\n%s", out)
	}
	// Redacting must not touch the live config.
	if c.MQTT.Password != "change-me" {
		t.Errorf("Redacted modified the config: password = %q", c.MQTT.Password)
	}

	empty := (&Config{MQTT: MQTTConfig{Broker: "tcp://b:1883"}}).Redacted()
	if strings.Contains(empty, "***") {
		t.Errorf("no password should mean no mask:\n%s", empty)
	}
}

func TestResolvePassword(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mqtt_password")
	if err := os.WriteFile(file, []byte("  s3cret\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := MQTTConfig{Password: "inline", PasswordFile: file}
	got, err := m.ResolvePassword()
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("ResolvePassword = %q, want trimmed file content", got)
	}

	m = MQTTConfig{Password: "inline"}
	if got, err := m.ResolvePassword(); err != nil || got != "inline" {
		t.Errorf("ResolvePassword without file = %q, %v", got, err)
	}

	m = MQTTConfig{PasswordFile: filepath.Join(dir, "missing")}
	if _, err := m.ResolvePassword(); err == nil || !strings.Contains(err.Error(), "mqtt.password_file") {
		t.Errorf("missing password file should error with context, got %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got, want := DefaultPath(), filepath.Join(home, ".config", "systemd2mqtt", "config.yaml"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultPath(), filepath.Join("/xdg", "systemd2mqtt", "config.yaml"); got != want {
		t.Errorf("DefaultPath with XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	t.Setenv("SYSTEMD2MQTT_CONFIG", "/etc/s2m.yaml")
	if got := DefaultPath(); got != "/etc/s2m.yaml" {
		t.Errorf("DefaultPath with SYSTEMD2MQTT_CONFIG = %q", got)
	}
}
