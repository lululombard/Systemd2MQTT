package mqtt

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

func testCfg() config.MQTTConfig {
	return config.MQTTConfig{
		Broker:    "tcp://127.0.0.1:1",
		ClientID:  "systemd2mqtt-test",
		BaseTopic: "systemd2mqtt",
		KeepAlive: 30 * time.Second,
		QoS:       1,
	}
}

func TestNewWarnsWhenTLSFieldsIgnored(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := testCfg()
	cfg.TLS.CAFile = "/etc/ssl/certs/ca-certificates.crt"
	cl, err := New(cfg, "systemd2mqtt/test/availability", nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if cl == nil {
		t.Fatal("nil client")
	}
	if !strings.Contains(buf.String(), "mqtt.tls.* is ignored") {
		t.Errorf("expected a warning about ignored tls fields, got: %q", buf.String())
	}
	if cl.IsConnected() {
		t.Error("New must not connect")
	}
	if cl.Reconnects() != 0 {
		t.Error("Reconnects should start at 0")
	}
}

func TestNewNoWarningWithoutTLSFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := New(testCfg(), "systemd2mqtt/test/availability", nil, nil, log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "mqtt.tls") {
		t.Errorf("unexpected tls warning: %q", buf.String())
	}
}

func TestNewBadPasswordFile(t *testing.T) {
	cfg := testCfg()
	cfg.PasswordFile = filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := New(cfg, "systemd2mqtt/test/availability", nil, nil, nil); err == nil {
		t.Fatal("expected an error for a missing password_file")
	} else if !strings.Contains(err.Error(), "password_file") {
		t.Errorf("error should mention password_file: %v", err)
	}
}

func TestNewBadTLSConfigFails(t *testing.T) {
	cfg := testCfg()
	cfg.Broker = "ssl://127.0.0.1:1"
	cfg.TLS.CAFile = filepath.Join(t.TempDir(), "missing-ca.pem")
	if _, err := New(cfg, "systemd2mqtt/test/availability", nil, nil, nil); err == nil {
		t.Fatal("expected an error for a missing ca_file with an ssl:// broker")
	}
}

func TestSubscribeRecordsFilterWhileOffline(t *testing.T) {
	cl, err := New(testCfg(), "systemd2mqtt/test/availability", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	cl.Subscribe("systemd2mqtt/test/unit/+/set", func(string, []byte, bool) { called = true })
	cl.Subscribe("homeassistant/status", func(string, []byte, bool) {})
	cl.mu.Lock()
	n := len(cl.subs)
	cl.mu.Unlock()
	if n != 2 {
		t.Fatalf("recorded %d filters, want 2", n)
	}
	// Publishing while offline must be a no-op that returns immediately.
	cl.Publish("systemd2mqtt/test/x", []byte("1"), true)
	if called {
		t.Error("handler must not run without a message")
	}
	// Close on a never-connected client must not block or panic.
	done := make(chan struct{})
	go func() { cl.Close("systemd2mqtt/test/availability"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a never-connected client")
	}
}

func TestNewWithoutWillTopicDisablesWill(t *testing.T) {
	cl, err := New(testCfg(), "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r0 := cl.c.OptionsReader()
	if r0.WillEnabled() {
		t.Error("an empty will topic must not enable the will")
	}
	cl2, err := New(testCfg(), "systemd2mqtt/test/availability", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := cl2.c.OptionsReader()
	if !r.WillEnabled() || r.WillTopic() != "systemd2mqtt/test/availability" || string(r.WillPayload()) != "offline" || !r.WillRetained() {
		t.Errorf("will not configured: enabled=%v topic=%q payload=%q retained=%v", r.WillEnabled(), r.WillTopic(), r.WillPayload(), r.WillRetained())
	}
	if !r.Order() {
		t.Error("OrderMatters must stay on so commands on one topic arrive in publish order")
	}
	if r.WriteTimeout() != publishTimeout {
		t.Errorf("WriteTimeout = %s, want %s", r.WriteTimeout(), publishTimeout)
	}
}

func TestPublishNeverBlocksAndCloseIsBounded(t *testing.T) {
	cl, err := New(testCfg(), "systemd2mqtt/test/availability", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Far more than the queue can hold, all while disconnected: every call
	// must return right away, the pump drops what it cannot send.
	start := time.Now()
	for i := 0; i < 3*publishQueueSize; i++ {
		cl.Publish("systemd2mqtt/test/x", []byte("1"), true)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Publish blocked: %d calls took %s", 3*publishQueueSize, d)
	}
	done := make(chan struct{})
	go func() { cl.Close("systemd2mqtt/test/availability"); close(done) }()
	select {
	case <-done:
	case <-time.After(offlineTimeout + 2*time.Second):
		t.Fatal("Close did not return within its bound")
	}
	// Publish after Close is a no-op, not a panic.
	cl.Publish("systemd2mqtt/test/x", []byte("1"), true)
	select {
	case <-cl.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop after Close")
	}
	// A second Close is harmless.
	cl.Close("")
}

func TestTopicMatches(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"a/+/set", "a/vlc/set", true},
		{"a/+/set", "a/vlc/cam1/set", false},
		{"a/+", "a/", true},
		{"a/#", "a", true},
		{"a/#", "a/b/c", true},
		{"#", "anything/at/all", true},
		{"a", "a/b", false},
		{"a/b", "a", false},
		{"+/status", "homeassistant/status", true},
	}
	for _, c := range cases {
		if got := topicMatches(c.filter, c.topic); got != c.want {
			t.Errorf("topicMatches(%q, %q) = %v, want %v", c.filter, c.topic, got, c.want)
		}
	}
}
