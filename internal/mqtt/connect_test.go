package mqtt

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// A broker that refuses the connection must show up in the log as a warning, not
// disappear into paho's debug output.
func TestConnectFailureIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.MQTTConfig{Broker: "tcp://127.0.0.1:1", ClientID: "t", KeepAlive: 30 * time.Second}
	cl, err := New(cfg, "", nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close("")
	if err := cl.Connect(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "mqtt connect failed, retrying") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "mqtt connect failed, retrying") || !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN about the failed connect, got:\n%s", out)
	}
	if cl.IsConnected() {
		t.Fatal("must not report connected")
	}
}
