// Command systemd2mqtt exposes a whitelist of systemd units and the display power state
// to Home Assistant over MQTT.
//
// Exit codes: 0 clean, 1 runtime error, 2 config error. The user unit sets
// RestartPreventExitStatus=2 so a broken config does not restart in a loop.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/app"
	"github.com/lululombard/Systemd2MQTT/internal/config"
	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/ha"
	"github.com/lululombard/Systemd2MQTT/internal/mqtt"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
	"github.com/lululombard/Systemd2MQTT/internal/version"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

// clearConnectTimeout is how long --clear-discovery waits for the broker.
const clearConnectTimeout = 15 * time.Second

// clearFlushDelay gives paho time to push the blanking publishes out before we disconnect.
const clearFlushDelay = 2 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the parsed command line flags.
type options struct {
	configPath      string
	version         bool
	checkConfig     bool
	dumpDiscovery   bool
	printPolkitRule bool
	clearDiscovery  bool
}

// parseFlags parses args into options. Usage output goes to stderr.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("systemd2mqtt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.configPath, "config", config.DefaultPath(), "path to the YAML config file")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")
	fs.BoolVar(&o.checkConfig, "check-config", false, "load and validate the config, print it with secrets masked, then exit")
	fs.BoolVar(&o.dumpDiscovery, "dump-discovery", false, "print the Home Assistant discovery payload as JSON and exit")
	fs.BoolVar(&o.printPolkitRule, "print-polkit-rule", false, "print the polkit rule for the configured system-scope units and exit")
	fs.BoolVar(&o.clearDiscovery, "clear-discovery", false, "blank every retained topic of this node on the broker and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "systemd2mqtt: %v\n", err)
		return exitConfig
	}
	if o.version {
		fmt.Fprintln(stdout, version.String())
		return exitOK
	}

	cfg, err := config.Load(o.configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return exitConfig
	}
	log := newLogger(stderr, cfg.LogLevel)
	// Leaf packages (systemd supervisor, display controller) log through
	// slog.Default, so point it at the same handler for one consistent stream.
	slog.SetDefault(log)
	t := ha.NewTopics(cfg)

	switch {
	case o.checkConfig:
		fmt.Fprint(stdout, cfg.Redacted())
		return exitOK
	case o.dumpDiscovery:
		return dumpDiscovery(cfg, t, stdout, stderr)
	case o.printPolkitRule:
		fmt.Fprint(stdout, ha.RenderPolkitRule(cfg, currentUser()))
		return exitOK
	}

	// The modes above never touch files, so CI can run them on the example config.
	// From here on the broker is needed, so read the file-backed settings now and
	// treat a failure as a config error: a typo in password_file or ca_file must
	// exit 2 and stop the unit, not restart it every 3 s forever.
	if err := preflight(cfg); err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return exitConfig
	}
	if o.clearDiscovery {
		return clearDiscovery(cfg, t, log, stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := app.Deps{
		Systemd: systemd.DefaultDialer,
		Display: display.DefaultDialer,
		MQTT: func(will string, onConnect func(), onLost func(error)) (app.MQTTClient, error) {
			return mqtt.New(cfg.MQTT, will, onConnect, onLost, log)
		},
		Log:     log,
		Version: version.Short(),
		Commit:  version.Commit,
	}
	a, err := app.New(cfg, deps)
	if err != nil {
		log.Error("startup failed", "err", err)
		return exitRuntime
	}
	if err := a.Run(ctx); err != nil {
		log.Error("daemon stopped with error", "err", err)
		return exitRuntime
	}
	return exitOK
}

// preflight reads the files the MQTT client will need (password_file, TLS
// files) so a missing or unreadable one is reported as a config error before
// anything connects. config.Load does not do this on purpose.
func preflight(cfg *config.Config) error {
	if _, err := cfg.MQTT.ResolvePassword(); err != nil {
		return err
	}
	if cfg.MQTT.UsesTLS() {
		if _, err := mqtt.BuildTLSConfig(cfg.MQTT.TLS); err != nil {
			return err
		}
	}
	return nil
}

// newLogger builds a text logger on stderr at the configured level. Unknown levels
// cannot happen after config.Load, but fall back to info anyway.
func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l}))
}

// currentUser is the login name the polkit rule should allow: $USER first, then the passwd entry.
func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "lululombard"
}

func dumpDiscovery(cfg *config.Config, t ha.Topics, stdout, stderr io.Writer) int {
	raw, err := ha.Marshal(ha.BuildDiscovery(cfg, t, version.Short()))
	if err != nil {
		fmt.Fprintf(stderr, "discovery error: %v\n", err)
		return exitRuntime
	}
	// Indent the bytes Marshal produced so key order and escaping stay exactly as published.
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Fprintf(stderr, "discovery error: %v\n", err)
		return exitRuntime
	}
	buf.WriteByte('\n')
	_, _ = stdout.Write(buf.Bytes())
	return exitOK
}

// clearDiscovery publishes an empty retained payload to every topic the daemon retains,
// which removes the device from Home Assistant and leaves nothing behind on the broker.
//
// It connects with its own client id and no will: reusing the daemon's id would make
// the broker kick a running daemon (which then reconnects and republishes), and a will
// on R/availability would put a retained "offline" right back on a topic we just blanked.
func clearDiscovery(cfg *config.Config, t ha.Topics, log *slog.Logger, stderr io.Writer) int {
	connected := make(chan struct{}, 1)
	onConnect := func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	}
	onLost := func(err error) { log.Warn("mqtt connection lost", "err", err) }

	mcfg := cfg.MQTT
	mcfg.ClientID = cfg.MQTT.ClientID + "-clear"
	cl, err := mqtt.New(mcfg, "", onConnect, onLost, log)
	if err != nil {
		fmt.Fprintf(stderr, "mqtt error: %v\n", err)
		return exitRuntime
	}
	if err := cl.Connect(); err != nil {
		fmt.Fprintf(stderr, "mqtt error: %v\n", err)
		return exitRuntime
	}
	if !waitConnected(cl, connected, clearConnectTimeout) {
		fmt.Fprintf(stderr, "mqtt error: not connected to %s after %s\n", cfg.MQTT.Broker, clearConnectTimeout)
		cl.Close("")
		return exitRuntime
	}

	topics := t.RetainedTopics(cfg)
	for _, topic := range topics {
		cl.Publish(topic, nil, true)
	}
	log.Info("cleared retained topics", "count", len(topics), "node_id", cfg.NodeID)
	time.Sleep(clearFlushDelay)
	// Close with an empty availability topic: we just blanked it and must not republish "offline".
	cl.Close("")
	return exitOK
}

// waitConnected polls IsConnected (and listens for the connect callback) up to timeout.
func waitConnected(cl *mqtt.Client, connected <-chan struct{}, timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if cl.IsConnected() {
			return true
		}
		select {
		case <-connected:
			return true
		case <-tick.C:
		case <-deadline:
			return cl.IsConnected()
		}
	}
}
