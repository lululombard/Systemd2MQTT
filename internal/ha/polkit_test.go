package ha

import (
	"strings"
	"testing"
)

func TestRenderPolkitRuleNoSystemUnits(t *testing.T) {
	cfg := loadTestConfig(t, "office-kiosk.yaml")
	out := RenderPolkitRule(cfg, "lululombard")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "//") {
			t.Errorf("expected a comment-only file, got line %q", line)
		}
	}
	if !strings.Contains(out, "No unit uses scope: system") {
		t.Errorf("missing explanation:\n%s", out)
	}
	if strings.Contains(out, "polkit.addRule") {
		t.Error("must not add a rule without system units")
	}
}

func TestRenderPolkitRulePlainSystemUnit(t *testing.T) {
	cfg := parseTestConfig(t, `
node_id: box
mqtt: { broker: tcp://localhost:1883 }
units:
  - { name: zebra.service, scope: system }
  - { name: apple.service, scope: system }
  - { name: grafana-kiosk.service, scope: user }
`)
	out := RenderPolkitRule(cfg, "lululombard")
	for _, want := range []string{
		`polkit.addRule(function(action, subject) {`,
		`action.id !== "org.freedesktop.systemd1.manage-units"`,
		`var allowedUser  = "lululombard";`,
		`var allowedUnits = ["apple.service", "zebra.service"];`,
		`var allowedVerbs = ["start", "stop", "restart"];`,
		`action.lookup("unit")`,
		`action.lookup("verb")`,
		`subject.user !== allowedUser`,
		`allowedUnits.indexOf(unit) !== -1`,
		`return polkit.Result.YES;`,
		`return polkit.Result.NOT_HANDLED;`,
		`/etc/polkit-1/rules.d/`,
		`reloads on its own`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "grafana-kiosk") {
		t.Error("user units must not appear in the rule")
	}
	if strings.Contains(out, "indexOf(") && strings.Contains(out, ".slice(-") {
		t.Error("no prefix match expected without a system template")
	}
	for _, es6 := range []string{"let ", "const ", "=>", "includes(", "startsWith(", "endsWith("} {
		if strings.Contains(out, es6) {
			t.Errorf("rule must be ES5, found %q", es6)
		}
	}
}

func TestRenderPolkitRuleSystemTemplate(t *testing.T) {
	cfg := parseTestConfig(t, `
node_id: box
mqtt: { broker: tcp://localhost:1883 }
units: []
templates:
  - name: vlc@.service
    scope: system
    instances: [cam1, cam2]
  - name: getty@.service
    scope: user
    instances: [tty9]
`)
	out := RenderPolkitRule(cfg, "kiosk")
	for _, want := range []string{
		`var allowedUser  = "kiosk";`,
		`var allowedUnits = [];`,
		`unit.indexOf("vlc@") === 0 && unit.slice(-8) === ".service"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "vlc@cam1") {
		t.Error("template instances must match by prefix, not by exact name")
	}
	if strings.Contains(out, "getty@") {
		t.Error("user-scope templates must not appear in the rule")
	}
}
