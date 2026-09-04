// Package ha builds the MQTT topic layout and Home Assistant discovery payloads.
package ha

import (
	"strings"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// Topics builds every topic from the base topic and node id.
type Topics struct {
	Base            string
	NodeID          string
	DiscoveryPrefix string
	StatusTopic     string
}

func NewTopics(cfg *config.Config) Topics {
	return Topics{
		Base:            cfg.MQTT.BaseTopic,
		NodeID:          cfg.NodeID,
		DiscoveryPrefix: cfg.HomeAssistant.DiscoveryPrefix,
		StatusTopic:     cfg.HomeAssistant.StatusTopic,
	}
}

func (t Topics) root() string { return t.Base + "/" + t.NodeID }

func (t Topics) Availability() string { return t.root() + "/availability" }
func (t Topics) BusAvailability(s config.Scope) string {
	return t.root() + "/bus/" + string(s) + "/availability"
}
func (t Topics) UnitState(id string) string      { return t.root() + "/unit/" + id + "/state" }
func (t Topics) UnitSet(id string) string        { return t.root() + "/unit/" + id + "/set" }
func (t Topics) UnitAttributes(id string) string { return t.root() + "/unit/" + id + "/attributes" }
func (t Topics) UnitSetWildcard() string         { return t.root() + "/unit/+/set" }
func (t Topics) TemplateState(id string) string  { return t.root() + "/template/" + id + "/state" }
func (t Topics) TemplateSet(id string) string    { return t.root() + "/template/" + id + "/set" }
func (t Topics) TemplateSetWildcard() string     { return t.root() + "/template/+/set" }
func (t Topics) DisplayState() string            { return t.root() + "/display/state" }
func (t Topics) DisplaySet() string              { return t.root() + "/display/set" }
func (t Topics) DisplayModeState() string        { return t.root() + "/display/mode/state" }
func (t Topics) DisplayModeSet() string          { return t.root() + "/display/mode/set" }
func (t Topics) DisplayAttributes() string       { return t.root() + "/display/attributes" }
func (t Topics) DisplayAvailability() string     { return t.root() + "/display/availability" }
func (t Topics) DaemonState() string             { return t.root() + "/daemon/state" }
func (t Topics) DaemonRestart() string           { return t.root() + "/daemon/restart" }
func (t Topics) DiscoveryConfig() string {
	return t.DiscoveryPrefix + "/device/" + t.NodeID + "/config"
}

// InboundKind classifies a subscribed topic.
type InboundKind int

const (
	InUnknown InboundKind = iota
	InUnitSet
	InTemplateSet
	InDisplaySet
	InDisplayModeSet
	InDaemonRestart
	InHAStatus
)

// Inbound is a parsed subscribed topic.
type Inbound struct {
	Kind InboundKind
	// ID is the unit id for InUnitSet or the template id for InTemplateSet.
	ID string
}

// Parse maps a received topic to what it means.
func (t Topics) Parse(topic string) (Inbound, bool) {
	if topic == t.StatusTopic {
		return Inbound{Kind: InHAStatus}, true
	}
	switch topic {
	case t.DisplaySet():
		return Inbound{Kind: InDisplaySet}, true
	case t.DisplayModeSet():
		return Inbound{Kind: InDisplayModeSet}, true
	case t.DaemonRestart():
		return Inbound{Kind: InDaemonRestart}, true
	}
	rest, ok := strings.CutPrefix(topic, t.root()+"/")
	if !ok {
		return Inbound{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != "set" || parts[1] == "" {
		return Inbound{}, false
	}
	switch parts[0] {
	case "unit":
		return Inbound{Kind: InUnitSet, ID: parts[1]}, true
	case "template":
		return Inbound{Kind: InTemplateSet, ID: parts[1]}, true
	}
	return Inbound{}, false
}

// Subscriptions lists every filter the daemon subscribes to.
func (t Topics) Subscriptions(cfg *config.Config) []string {
	subs := []string{t.UnitSetWildcard(), t.TemplateSetWildcard(), t.DaemonRestart()}
	if cfg.DisplayEnabled() {
		subs = append(subs, t.DisplaySet(), t.DisplayModeSet())
	}
	if cfg.HAEnabled() {
		subs = append(subs, t.StatusTopic)
	}
	return subs
}

// RetainedTopics lists everything --clear-discovery blanks, discovery config included.
func (t Topics) RetainedTopics(cfg *config.Config) []string {
	out := []string{t.DiscoveryConfig(), t.Availability(), t.DaemonState()}
	for _, s := range cfg.Scopes() {
		out = append(out, t.BusAvailability(s))
	}
	for _, u := range cfg.AllUnits() {
		id := u.ObjectID()
		out = append(out, t.UnitState(id), t.UnitAttributes(id))
	}
	for _, s := range cfg.Selects() {
		out = append(out, t.TemplateState(s.ID))
	}
	out = append(out, t.DisplayState(), t.DisplayModeState(), t.DisplayAttributes(), t.DisplayAvailability())
	return out
}
