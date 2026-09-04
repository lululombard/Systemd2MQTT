package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lululombard/Systemd2MQTT/internal/version"
)

func TestParseFlagsDefaults(t *testing.T) {
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.configPath == "" {
		t.Fatal("default config path is empty")
	}
	if o.version || o.checkConfig || o.dumpDiscovery || o.printPolkitRule || o.clearDiscovery {
		t.Fatalf("no mode flag should be set by default: %+v", o)
	}
}

func TestParseFlagsAll(t *testing.T) {
	o, err := parseFlags([]string{"-config", "/x/y.yaml", "-check-config", "-dump-discovery", "-print-polkit-rule", "-clear-discovery", "-version"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.configPath != "/x/y.yaml" {
		t.Errorf("configPath = %q", o.configPath)
	}
	if !(o.version && o.checkConfig && o.dumpDiscovery && o.printPolkitRule && o.clearDiscovery) {
		t.Errorf("flags not all set: %+v", o)
	}
}

func TestParseFlagsDoubleDash(t *testing.T) {
	o, err := parseFlags([]string{"--config", "a.yaml", "--version"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.configPath != "a.yaml" || !o.version {
		t.Errorf("unexpected options: %+v", o)
	}
}

func TestRunUnknownFlagIsConfigError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-bogus"}, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Errorf("stderr should mention the bad flag: %q", stderr.String())
	}
}

func TestRunPositionalArgIsConfigError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"extra"}, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
}

func TestRunHelpIsClean(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr.String(), "-config") {
		t.Errorf("usage should list -config: %q", stderr.String())
	}
}

func TestRunVersionSkipsConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A config path that does not exist proves -version never loads the config.
	code := run([]string{"-config", "/nonexistent/systemd2mqtt.yaml", "-version"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version.String() {
		t.Errorf("stdout = %q, want %q", got, version.String())
	}
}

func TestRunMissingConfigIsConfigError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", "/nonexistent/systemd2mqtt.yaml", "-check-config"}, &stdout, &stderr)
	if code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if !strings.HasPrefix(stderr.String(), "config error: ") {
		t.Errorf("stderr = %q, want a config error prefix", stderr.String())
	}
}

func TestRunInvalidConfigIsConfigError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mqtt:\n  broker: tcp://localhost:1883\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", path, "-check-config"}, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitConfig, stderr.String())
	}
}

func TestRunMissingPasswordFileIsConfigError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "node_id: testnode\nmqtt:\n  broker: tcp://localhost:1883\n  password_file: " + filepath.Join(dir, "mqtt_pasword") + "\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	// --check-config never touches files, so it still passes.
	if code := run([]string{"-config", path, "-check-config"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("check-config exit code = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	// The daemon and --clear-discovery need the file and must exit 2, not 1, so
	// RestartPreventExitStatus=2 stops the unit instead of looping.
	for _, args := range [][]string{{"-config", path}, {"-config", path, "-clear-discovery"}} {
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != exitConfig {
			t.Fatalf("%v: exit code = %d, want %d (stderr %q)", args, code, exitConfig, stderr.String())
		}
		if !strings.Contains(stderr.String(), "config error: mqtt.password_file") {
			t.Errorf("%v: stderr = %q, want a password_file config error", args, stderr.String())
		}
	}
}

func TestRunMissingCAFileIsConfigError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "node_id: testnode\nmqtt:\n  broker: ssl://localhost:8883\n  tls:\n    ca_file: " + filepath.Join(dir, "missing-ca.pem") + "\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", path}, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitConfig, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "config error: ") {
		t.Errorf("stderr = %q, want a config error prefix", stderr.String())
	}
}

func TestRunCheckConfigRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node_id: testnode\nmqtt:\n  broker: tcp://localhost:1883\n  password: hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", path, "-check-config"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "hunter2") {
		t.Errorf("password leaked into --check-config output: %q", out)
	}
	if !strings.Contains(out, "testnode") {
		t.Errorf("node_id missing from --check-config output: %q", out)
	}
}

func TestRunDumpDiscoveryIsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node_id: testnode\nmqtt:\n  broker: tcp://localhost:1883\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", path, "-dump-discovery"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "{\n") || !strings.Contains(out, "systemd2mqtt_testnode") {
		t.Errorf("unexpected discovery output: %q", out)
	}
}

func TestRunPrintPolkitRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node_id: testnode\nmqtt:\n  broker: tcp://localhost:1883\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USER", "kioskuser")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", path, "-print-polkit-rule"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Error("polkit rule output is empty")
	}
}

func TestCurrentUserPrefersEnv(t *testing.T) {
	t.Setenv("USER", "someone")
	if got := currentUser(); got != "someone" {
		t.Errorf("currentUser() = %q, want someone", got)
	}
	t.Setenv("USER", "")
	if got := currentUser(); got == "" {
		t.Error("currentUser() should fall back to the passwd entry")
	}
}
