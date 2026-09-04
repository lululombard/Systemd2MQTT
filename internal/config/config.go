// Package config loads and validates the YAML configuration and expands template units.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Scope selects which systemd manager owns a unit.
type Scope string

const (
	ScopeUser   Scope = "user"
	ScopeSystem Scope = "system"
)

// Config is the whole configuration file.
type Config struct {
	NodeID        string           `yaml:"node_id"`
	Device        DeviceConfig     `yaml:"device"`
	MQTT          MQTTConfig       `yaml:"mqtt"`
	HomeAssistant HAConfig         `yaml:"homeassistant"`
	Units         []UnitConfig     `yaml:"units"`
	Templates     []TemplateConfig `yaml:"templates"`
	Display       DisplayConfig    `yaml:"display"`
	Systemd       SystemdConfig    `yaml:"systemd"`
	LogLevel      string           `yaml:"log_level"`

	all     []UnitConfig
	selects []TemplateSelect
}

type DeviceConfig struct {
	Name          string `yaml:"name"`
	Manufacturer  string `yaml:"manufacturer"`
	Model         string `yaml:"model"`
	SuggestedArea string `yaml:"suggested_area"`
}

type MQTTConfig struct {
	Broker       string        `yaml:"broker"`
	ClientID     string        `yaml:"client_id"`
	Username     string        `yaml:"username"`
	Password     string        `yaml:"password"`
	PasswordFile string        `yaml:"password_file"`
	BaseTopic    string        `yaml:"base_topic"`
	KeepAlive    time.Duration `yaml:"keepalive"`
	QoS          byte          `yaml:"qos"`
	TLS          TLSConfig     `yaml:"tls"`

	// qosSet records whether the file spelled out mqtt.qos. A plain byte cannot
	// tell an explicit 0 from "not set", and the default is 1, so Parse peeks at
	// the raw YAML and applyDefaults only fills in the default when this is false.
	qosSet bool
}

// TLSConfig has no enabled flag on purpose: TLS is used iff the broker scheme says so.
type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

type HAConfig struct {
	Enabled         *bool  `yaml:"enabled"`
	DiscoveryPrefix string `yaml:"discovery_prefix"`
	StatusTopic     string `yaml:"status_topic"`
}

// UnitConfig is one unit exposed to Home Assistant. Template instances are expanded into these.
type UnitConfig struct {
	Name          string `yaml:"name"`
	Scope         Scope  `yaml:"scope"`
	ID            string `yaml:"id"`
	FriendlyName  string `yaml:"friendly_name"`
	Icon          string `yaml:"icon"`
	RestartButton *bool  `yaml:"restart_button"`
	ProblemSensor *bool  `yaml:"problem_sensor"`

	// TemplateID is set when this unit was expanded from a template.
	TemplateID string `yaml:"-"`
}

// TemplateConfig expands a systemd template unit (vlc@.service) into one UnitConfig per instance.
type TemplateConfig struct {
	Name          string           `yaml:"name"`
	Scope         Scope            `yaml:"scope"`
	ID            string           `yaml:"id"`
	FriendlyName  string           `yaml:"friendly_name"`
	Icon          string           `yaml:"icon"`
	Exclusive     bool             `yaml:"exclusive"`
	RestartButton *bool            `yaml:"restart_button"`
	ProblemSensor *bool            `yaml:"problem_sensor"`
	Instances     []InstanceConfig `yaml:"instances"`
}

// InstanceConfig accepts either a bare string ("cam1") or a mapping with name and friendly_name.
type InstanceConfig struct {
	Name         string `yaml:"name"`
	FriendlyName string `yaml:"friendly_name"`
}

func (i *InstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		i.Name = value.Value
		return nil
	}
	type raw InstanceConfig
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*i = InstanceConfig(r)
	return nil
}

// TemplateSelect describes the "pick one instance" select entity of an exclusive template.
type TemplateSelect struct {
	ID      string
	Name    string
	Icon    string
	Scope   Scope
	Options []SelectOption
}

// SelectOption maps a Home Assistant option label to an instance unit.
type SelectOption struct {
	Label    string
	UnitID   string
	UnitName string
}

type DisplayConfig struct {
	Enabled          *bool  `yaml:"enabled"`
	OffMode          string `yaml:"off_mode"`
	ExposeModeSelect *bool  `yaml:"expose_mode_select"`
}

type SystemdConfig struct {
	PollInterval time.Duration `yaml:"poll_interval"`
	JobTimeout   time.Duration `yaml:"job_timeout"`
	JobMode      string        `yaml:"job_mode"`
}

// DefaultPath is where the daemon looks for its config when --config is not given.
func DefaultPath() string {
	if p := os.Getenv("SYSTEMD2MQTT_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd2mqtt", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "systemd2mqtt", "config.yaml")
}

// Load reads, defaults, expands and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse is Load for in-memory YAML.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, os.ErrNotExist) {
		if err.Error() == "EOF" {
			return nil, errors.New("config file is empty")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.MQTT.qosSet = qosPresent(data)
	c.applyDefaults()
	c.applyEnv()
	if err := c.expand(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func boolPtr(b bool) *bool { return &b }

// qosPresent reports whether mqtt.qos appears in the raw YAML. It uses a lenient
// second decode of just that key; unknown keys are still rejected by the strict
// decode in Parse.
func qosPresent(data []byte) bool {
	var peek struct {
		MQTT struct {
			QoS *byte `yaml:"qos"`
		} `yaml:"mqtt"`
	}
	if err := yaml.Unmarshal(data, &peek); err != nil {
		return false
	}
	return peek.MQTT.QoS != nil
}

func (c *Config) applyDefaults() {
	if c.NodeID == "" {
		host, _ := os.Hostname()
		c.NodeID = sanitizeNodeID(host)
	}
	if c.Device.Name == "" {
		c.Device.Name = c.NodeID
	}
	if c.Device.Manufacturer == "" {
		c.Device.Manufacturer = "lululombard"
	}
	if c.Device.Model == "" {
		c.Device.Model = "Systemd2MQTT"
	}
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = "systemd2mqtt-" + c.NodeID
	}
	if c.MQTT.BaseTopic == "" {
		c.MQTT.BaseTopic = "systemd2mqtt"
	}
	if c.MQTT.KeepAlive == 0 {
		c.MQTT.KeepAlive = 30 * time.Second
	}
	if !c.MQTT.qosSet {
		c.MQTT.QoS = 1
	}
	if c.HomeAssistant.Enabled == nil {
		c.HomeAssistant.Enabled = boolPtr(true)
	}
	if c.HomeAssistant.DiscoveryPrefix == "" {
		c.HomeAssistant.DiscoveryPrefix = "homeassistant"
	}
	if c.HomeAssistant.StatusTopic == "" {
		c.HomeAssistant.StatusTopic = "homeassistant/status"
	}
	// Defaults only when neither key is present at all; an explicit empty list means no units.
	if c.Units == nil && c.Templates == nil {
		c.Units = []UnitConfig{
			{Name: "grafana-kiosk.service", Scope: ScopeUser},
			{Name: "vlc.service", Scope: ScopeUser},
		}
	}
	if c.Display.Enabled == nil {
		c.Display.Enabled = boolPtr(true)
	}
	if c.Display.OffMode == "" {
		c.Display.OffMode = "off"
	}
	if c.Display.ExposeModeSelect == nil {
		c.Display.ExposeModeSelect = boolPtr(true)
	}
	if c.Systemd.PollInterval == 0 {
		c.Systemd.PollInterval = 30 * time.Second
	}
	if c.Systemd.JobTimeout == 0 {
		c.Systemd.JobTimeout = 60 * time.Second
	}
	if c.Systemd.JobMode == "" {
		c.Systemd.JobMode = "replace"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	for i := range c.Units {
		if c.Units[i].Icon == "" {
			c.Units[i].Icon = "mdi:cog-play"
		}
	}
}

func (c *Config) applyEnv() {
	if v := os.Getenv("SYSTEMD2MQTT_MQTT_BROKER"); v != "" {
		c.MQTT.Broker = v
	}
	if v := os.Getenv("SYSTEMD2MQTT_MQTT_USERNAME"); v != "" {
		c.MQTT.Username = v
	}
	if v := os.Getenv("SYSTEMD2MQTT_MQTT_PASSWORD"); v != "" {
		c.MQTT.Password = v
	}
}

// expand turns templates into plain units and records the select entities.
func (c *Config) expand() error {
	c.all = slices.Clone(c.Units)
	c.selects = nil
	for ti := range c.Templates {
		t := &c.Templates[ti]
		prefix, suffix, ok := splitTemplate(t.Name)
		if !ok {
			return fmt.Errorf("templates[%d]: %q is not a template unit name (expected like vlc@.service)", ti, t.Name)
		}
		if t.ID == "" {
			t.ID = Slugify(t.Name)
		}
		if t.FriendlyName == "" {
			t.FriendlyName = displayName(prefix[:len(prefix)-1] + suffix)
		}
		if t.Icon == "" {
			t.Icon = "mdi:cog-play"
		}
		sel := TemplateSelect{ID: t.ID, Name: t.FriendlyName, Icon: t.Icon, Scope: t.Scope}
		for _, inst := range t.Instances {
			u := UnitConfig{
				Name:          prefix + inst.Name + suffix,
				Scope:         t.Scope,
				Icon:          t.Icon,
				RestartButton: t.RestartButton,
				ProblemSensor: t.ProblemSensor,
				TemplateID:    t.ID,
			}
			u.ID = Slugify(u.Name)
			if inst.FriendlyName != "" {
				u.FriendlyName = inst.FriendlyName
			} else {
				u.FriendlyName = t.FriendlyName + " " + inst.Name
			}
			c.all = append(c.all, u)
			sel.Options = append(sel.Options, SelectOption{Label: u.FriendlyName, UnitID: u.ID, UnitName: u.Name})
		}
		if t.Exclusive {
			c.selects = append(c.selects, sel)
		}
	}
	return nil
}

var nodeIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Validate checks the config without touching the filesystem or network.
func (c *Config) Validate() error {
	var errs []error
	if !nodeIDRe.MatchString(c.NodeID) {
		errs = append(errs, fmt.Errorf("node_id %q must match %s", c.NodeID, nodeIDRe))
	}
	if c.MQTT.Broker == "" {
		errs = append(errs, errors.New("mqtt.broker is required (e.g. tcp://host:1883)"))
	} else if u, err := url.Parse(c.MQTT.Broker); err != nil || u.Host == "" {
		errs = append(errs, fmt.Errorf("mqtt.broker %q is not a valid URL", c.MQTT.Broker))
	} else if !slices.Contains([]string{"tcp", "mqtt", "ssl", "tls", "mqtts", "ws", "wss"}, u.Scheme) {
		errs = append(errs, fmt.Errorf("mqtt.broker scheme %q must be one of tcp, mqtt, ssl, tls, mqtts, ws, wss", u.Scheme))
	}
	if c.MQTT.QoS > 2 {
		errs = append(errs, fmt.Errorf("mqtt.qos %d must be 0, 1 or 2", c.MQTT.QoS))
	}
	if (c.MQTT.TLS.CertFile == "") != (c.MQTT.TLS.KeyFile == "") {
		errs = append(errs, errors.New("mqtt.tls.cert_file and mqtt.tls.key_file must be set together"))
	}
	if c.MQTT.BaseTopic == "" || strings.ContainsAny(c.MQTT.BaseTopic, "+#") {
		errs = append(errs, fmt.Errorf("mqtt.base_topic %q must not be empty or contain wildcards", c.MQTT.BaseTopic))
	}
	seen := map[string]string{}
	// seenName catches the same unit listed twice in one scope under different
	// ids: the state lookup is by name, so only one of them would ever update.
	seenName := map[string]string{}
	// seenCmp mirrors the Home Assistant component ids discovery derives from
	// each unit id (unit_<id>, unit_<id>_state, ...). Two ids like "foo" and
	// "foo_state" would otherwise overwrite each other's entity silently.
	seenCmp := map[string]string{}
	for i, u := range c.all {
		where := fmt.Sprintf("units[%d]", i)
		if u.TemplateID != "" {
			where = fmt.Sprintf("template %q instance %q", u.TemplateID, u.Name)
		}
		if u.Name == "" || !strings.Contains(u.Name, ".") || strings.ContainsAny(u.Name, " \t/") {
			errs = append(errs, fmt.Errorf("%s: unit name %q must be a full unit name like foo.service", where, u.Name))
		}
		if u.TemplateID == "" {
			if _, _, isTpl := splitTemplate(u.Name); isTpl {
				// systemd's ListUnitsByNames fails as a whole on a template name,
				// which would make every query of that scope fail forever.
				errs = append(errs, fmt.Errorf("%s: %q is a template unit name, list it under templates: with instances, or use an instance name like %s",
					where, u.Name, strings.Replace(u.Name, "@.", "@cam1.", 1)))
			}
		}
		if u.Scope != ScopeUser && u.Scope != ScopeSystem {
			errs = append(errs, fmt.Errorf("%s: scope %q must be user or system", where, u.Scope))
		}
		id := u.ObjectID()
		if !nodeIDRe.MatchString(id) {
			errs = append(errs, fmt.Errorf("%s: id %q must match %s", where, id, nodeIDRe))
		}
		if prev, dup := seen[id]; dup {
			errs = append(errs, fmt.Errorf("%s: id %q is already used by %s", where, id, prev))
		}
		seen[id] = u.Name
		nameKey := string(u.Scope) + "/" + u.Name
		if prev, dup := seenName[nameKey]; dup {
			errs = append(errs, fmt.Errorf("%s: unit %q in scope %s is already configured as id %q", where, u.Name, u.Scope, prev))
		}
		seenName[nameKey] = id
		cmps := []string{"unit_" + id, "unit_" + id + "_state"}
		if u.WantsProblemSensor() {
			cmps = append(cmps, "unit_"+id+"_problem")
		}
		if u.WantsRestartButton() {
			cmps = append(cmps, "unit_"+id+"_restart")
		}
		for _, cmp := range cmps {
			if prev, dup := seenCmp[cmp]; dup && prev != id {
				errs = append(errs, fmt.Errorf("%s: id %q collides with id %q on Home Assistant component %q", where, id, prev, cmp))
			}
			seenCmp[cmp] = id
		}
	}
	seenTpl := map[string]int{}
	for i, t := range c.Templates {
		if prev, dup := seenTpl[t.ID]; dup {
			errs = append(errs, fmt.Errorf("templates[%d] (%s): id %q is already used by templates[%d]", i, t.Name, t.ID, prev))
		}
		seenTpl[t.ID] = i
		if len(t.Instances) == 0 {
			errs = append(errs, fmt.Errorf("templates[%d] (%s): instances must not be empty", i, t.Name))
		}
		for _, inst := range t.Instances {
			if inst.Name == "" || strings.ContainsAny(inst.Name, " \t/@") {
				errs = append(errs, fmt.Errorf("templates[%d] (%s): bad instance name %q", i, t.Name, inst.Name))
			}
		}
		if !nodeIDRe.MatchString(t.ID) {
			errs = append(errs, fmt.Errorf("templates[%d] (%s): id %q must match %s", i, t.Name, t.ID, nodeIDRe))
		}
	}
	if !slices.Contains([]string{"standby", "suspend", "off"}, c.Display.OffMode) {
		errs = append(errs, fmt.Errorf("display.off_mode %q must be standby, suspend or off", c.Display.OffMode))
	}
	if !slices.Contains([]string{"replace", "fail"}, c.Systemd.JobMode) {
		errs = append(errs, fmt.Errorf("systemd.job_mode %q must be replace or fail", c.Systemd.JobMode))
	}
	if c.Systemd.PollInterval < 5*time.Second {
		errs = append(errs, fmt.Errorf("systemd.poll_interval %s must be at least 5s", c.Systemd.PollInterval))
	}
	if c.Systemd.JobTimeout < 5*time.Second {
		errs = append(errs, fmt.Errorf("systemd.job_timeout %s must be at least 5s", c.Systemd.JobTimeout))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		errs = append(errs, fmt.Errorf("log_level %q must be debug, info, warn or error", c.LogLevel))
	}
	return errors.Join(errs...)
}

// AllUnits returns configured units plus expanded template instances, in config order.
func (c *Config) AllUnits() []UnitConfig { return c.all }

// Selects returns one entry per exclusive template.
func (c *Config) Selects() []TemplateSelect { return c.selects }

// UnitsForScope filters AllUnits by scope.
func (c *Config) UnitsForScope(s Scope) []UnitConfig {
	var out []UnitConfig
	for _, u := range c.all {
		if u.Scope == s {
			out = append(out, u)
		}
	}
	return out
}

// UnitByID looks a unit up by its object id.
func (c *Config) UnitByID(id string) (UnitConfig, bool) {
	for _, u := range c.all {
		if u.ObjectID() == id {
			return u, true
		}
	}
	return UnitConfig{}, false
}

// Scopes lists the scopes that have at least one unit, user first.
func (c *Config) Scopes() []Scope {
	var out []Scope
	for _, s := range []Scope{ScopeUser, ScopeSystem} {
		if len(c.UnitsForScope(s)) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) HAEnabled() bool      { return c.HomeAssistant.Enabled == nil || *c.HomeAssistant.Enabled }
func (c *Config) DisplayEnabled() bool { return c.Display.Enabled == nil || *c.Display.Enabled }
func (c *Config) ExposeModeSelect() bool {
	return c.Display.ExposeModeSelect == nil || *c.Display.ExposeModeSelect
}

// Redacted renders the resolved config as YAML with secrets masked.
func (c *Config) Redacted() string {
	cp := *c
	if cp.MQTT.Password != "" {
		cp.MQTT.Password = "***"
	}
	out, err := yaml.Marshal(&cp)
	if err != nil {
		return "# " + err.Error()
	}
	return string(out)
}

func (m MQTTConfig) UsesTLS() bool {
	u, err := url.Parse(m.Broker)
	if err != nil {
		return false
	}
	return slices.Contains([]string{"ssl", "tls", "mqtts", "wss"}, u.Scheme)
}

// ResolvePassword reads password_file when set, else returns password.
func (m MQTTConfig) ResolvePassword() (string, error) {
	if m.PasswordFile == "" {
		return m.Password, nil
	}
	b, err := os.ReadFile(m.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("mqtt.password_file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (u UnitConfig) ObjectID() string {
	if u.ID != "" {
		return u.ID
	}
	return Slugify(u.Name)
}

func (u UnitConfig) DisplayName() string {
	if u.FriendlyName != "" {
		return u.FriendlyName
	}
	return displayName(u.Name)
}

func (u UnitConfig) WantsRestartButton() bool { return u.RestartButton == nil || *u.RestartButton }
func (u UnitConfig) WantsProblemSensor() bool { return u.ProblemSensor == nil || *u.ProblemSensor }

// OffModeValue maps display.off_mode to a Mutter PowerSaveMode value.
func (d DisplayConfig) OffModeValue() int32 {
	switch d.OffMode {
	case "standby":
		return 1
	case "suspend":
		return 2
	default:
		return 3
	}
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a unit name into an object id: "vlc@cam1.service" becomes "vlc_cam1".
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.TrimSuffix(s, ".service")
	s = nonAlnum.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

func sanitizeNodeID(host string) string {
	s := strings.ToLower(host)
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "systemd2mqtt"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func displayName(unit string) string {
	s := strings.TrimSuffix(unit, ".service")
	if s == "" {
		return unit
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// splitTemplate splits "vlc@.service" into "vlc@" and ".service".
func splitTemplate(name string) (prefix, suffix string, ok bool) {
	i := strings.Index(name, "@.")
	if i <= 0 || i+2 >= len(name) {
		return "", "", false
	}
	return name[:i+1], name[i+1:], true
}
