package app

import (
	"log/slog"

	"github.com/lululombard/Systemd2MQTT/internal/display"
	"github.com/lululombard/Systemd2MQTT/internal/systemd"
)

// MQTTClient is the slice of the MQTT client the app needs. internal/mqtt.Client implements it.
type MQTTClient interface {
	// Connect kicks off the connection and returns immediately; retries happen in the background.
	Connect() error
	// Publish never blocks the caller.
	Publish(topic string, payload []byte, retained bool)
	// Subscribe registers a handler; the handler must not block.
	Subscribe(filter string, h func(topic string, payload []byte, retained bool))
	IsConnected() bool
	Reconnects() int64
	// Close publishes availability offline with a bounded wait, then disconnects.
	Close(availabilityTopic string)
}

// MQTTFactory builds the client once the app knows its will topic and callbacks.
type MQTTFactory func(willTopic string, onConnect func(), onLost func(error)) (MQTTClient, error)

// Deps are the injectable pieces of the app, real in main and fakes in tests.
type Deps struct {
	Systemd systemd.Dialer
	Display display.Dialer
	MQTT    MQTTFactory
	Log     *slog.Logger
	Version string
	Commit  string
}
