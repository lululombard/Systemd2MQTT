package ha

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// PolkitAction is the polkit action systemd checks for start/stop/restart.
const PolkitAction = "org.freedesktop.systemd1.manage-units"

// polkitVerbs is deliberately short: the daemon never reloads, kills or edits units.
var polkitVerbs = []string{"start", "stop", "restart"}

// RenderPolkitRule renders /etc/polkit-1/rules.d/10-systemd2mqtt.rules for the
// system-scope units in cfg, allowing user to start/stop/restart them without a
// prompt. With no system-scope unit it returns a comment-only file so the CI diff
// against deploy/10-systemd2mqtt.rules still works. ES5 only, polkitd runs duktape.
func RenderPolkitRule(cfg *config.Config, user string) string {
	var exact []string
	for _, u := range cfg.UnitsForScope(config.ScopeSystem) {
		if u.TemplateID == "" {
			exact = append(exact, u.Name)
		}
	}
	sort.Strings(exact)

	type prefixMatch struct{ prefix, suffix string }
	var prefixes []prefixMatch
	for _, t := range cfg.Templates {
		if t.Scope != config.ScopeSystem {
			continue
		}
		if i := strings.Index(t.Name, "@."); i > 0 && i+2 < len(t.Name) {
			prefixes = append(prefixes, prefixMatch{prefix: t.Name[:i+1], suffix: t.Name[i+1:]})
		}
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].prefix < prefixes[j].prefix })

	var b strings.Builder
	b.WriteString("// 10-systemd2mqtt.rules, rendered by: systemd2mqtt --print-polkit-rule\n")
	if len(exact) == 0 && len(prefixes) == 0 {
		b.WriteString("//\n")
		b.WriteString("// No unit uses scope: system, so no polkit rule is needed. User units are\n")
		b.WriteString("// managed over the session bus, which never asks polkit. Nothing to install.\n")
		return b.String()
	}

	b.WriteString("//\n")
	b.WriteString("// Install with:\n")
	b.WriteString("//   sudo install -m 0644 -o root -g root 10-systemd2mqtt.rules /etc/polkit-1/rules.d/\n")
	b.WriteString("// polkitd watches rules.d and reloads on its own, no restart needed. The 10-\n")
	b.WriteString("// prefix sorts before the distro's 49-/50- rules.\n")
	b.WriteString("//\n")
	fmt.Fprintf(&b, "// Lets %s start, stop and restart the system units listed below. polkit cannot\n", user)
	b.WriteString("// match executables, so every process running as that user gets the same rights.\n")
	b.WriteString("// Fine for a single-user kiosk. Re-render after changing scope: system units.\n")
	b.WriteString("polkit.addRule(function(action, subject) {\n")
	fmt.Fprintf(&b, "    if (action.id !== %s) { return polkit.Result.NOT_HANDLED; }\n", jsString(PolkitAction))
	fmt.Fprintf(&b, "    var allowedUser  = %s;\n", jsString(user))
	fmt.Fprintf(&b, "    var allowedUnits = %s;\n", jsArray(exact))
	fmt.Fprintf(&b, "    var allowedVerbs = %s;\n", jsArray(polkitVerbs))
	b.WriteString("    var unit = action.lookup(\"unit\"), verb = action.lookup(\"verb\");\n")
	b.WriteString("    if (subject.user !== allowedUser || typeof unit !== \"string\" || allowedVerbs.indexOf(verb) === -1) {\n")
	b.WriteString("        return polkit.Result.NOT_HANDLED;\n")
	b.WriteString("    }\n")
	b.WriteString("    if (allowedUnits.indexOf(unit) !== -1) { return polkit.Result.YES; }\n")
	for _, p := range prefixes {
		fmt.Fprintf(&b, "    // Any instance of %s%s\n", p.prefix, p.suffix)
		fmt.Fprintf(&b, "    if (unit.indexOf(%s) === 0 && unit.slice(-%d) === %s) { return polkit.Result.YES; }\n",
			jsString(p.prefix), len(p.suffix), jsString(p.suffix))
	}
	b.WriteString("    return polkit.Result.NOT_HANDLED;\n")
	b.WriteString("});\n")
	return b.String()
}

// jsString quotes s as a JS string literal. Go's escapes (\", \\, \uXXXX) are all valid ES5.
func jsString(s string) string { return strconv.Quote(s) }

func jsArray(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, jsString(s))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
