// Package mqtt wraps paho with the behaviour the daemon needs: TLS chosen by the broker
// URL scheme, a retained LWT, resubscribe on every (re)connect, a Publish that never
// blocks the caller and a bounded "offline" publish on Close.
//
// Paho's own Publish can block for up to WriteTimeout while the link is stalled (its
// outbound channel is unbuffered), so all publishes go through one ordered queue
// drained by a pump goroutine. The caller only ever does a non-blocking enqueue.
package mqtt

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

const (
	connectRetryInterval = 5 * time.Second
	maxReconnectInterval = 60 * time.Second
	subscribeTimeout     = 10 * time.Second
	publishTimeout       = 5 * time.Second
	offlineTimeout       = 3 * time.Second
	disconnectQuiesceMs  = 500
	// publishQueueSize bounds the outbound queue. A full republish is a few dozen
	// messages, so this only fills when the broker link is dead for a long time.
	publishQueueSize = 1024
)

// Handler receives one inbound message. It runs on paho's router goroutine, so it must
// hand the work off and return quickly.
type Handler func(topic string, payload []byte, retained bool)

// Client is the paho wrapper. Implements app.MQTTClient.
type Client struct {
	c   paho.Client
	qos byte
	log *slog.Logger

	onConnect func()
	onLost    func(error)

	mu   sync.Mutex
	subs map[string]Handler

	reconnects atomic.Int64

	out       chan outMsg
	stop      chan struct{}
	pumpDone  chan struct{}
	closeOnce sync.Once
}

// outMsg is one queued publish. done, when set, is closed once paho has acked
// the message or the wait gave up; only Close uses it.
type outMsg struct {
	topic    string
	payload  []byte
	retained bool
	qos      byte
	done     chan struct{}
}

// New builds a client but does not connect. willTopic gets a retained "offline" when
// the connection drops without a clean DISCONNECT. onConnect runs after every successful
// (re)connect once subscriptions are back in place, onLost after every drop.
func New(cfg config.MQTTConfig, willTopic string, onConnect func(), onLost func(error), log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	// Paho reports failed connect attempts (and the retry sleep) only through its
	// package-level loggers, so route those into slog once. Without this a dead
	// broker is completely silent at the default log level.
	pahoLogOnce.Do(func() {
		paho.ERROR = pahoLogger{log: log, level: slog.LevelWarn}
		paho.CRITICAL = pahoLogger{log: log, level: slog.LevelError}
	})
	pw, err := cfg.ResolvePassword()
	if err != nil {
		return nil, err
	}

	cl := &Client{
		qos:       cfg.QoS,
		log:       log,
		onConnect: onConnect,
		onLost:    onLost,
		subs:      map[string]Handler{},
		out:       make(chan outMsg, publishQueueSize),
		stop:      make(chan struct{}),
		pumpDone:  make(chan struct{}),
	}

	// OrderMatters stays on (paho's default): with it off every inbound message
	// gets its own goroutine and an OFF followed by an ON on the same set topic
	// could reach the app in the wrong order. Our handler never blocks, so the
	// serial delivery costs nothing.
	opts := paho.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(pw).
		SetKeepAlive(cfg.KeepAlive).
		SetCleanSession(true).
		SetOrderMatters(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(connectRetryInterval).
		SetMaxReconnectInterval(maxReconnectInterval).
		SetWriteTimeout(publishTimeout).
		SetOnConnectHandler(cl.handleConnect).
		SetConnectionLostHandler(cl.handleLost)
	if willTopic != "" {
		// SetWill with an empty topic would still enable the will and produce an
		// invalid CONNECT; --clear-discovery runs without one on purpose.
		opts.SetWill(willTopic, "offline", 1, true)
	}

	if cfg.UsesTLS() {
		tc, err := BuildTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tc)
	} else if tlsFieldsSet(cfg.TLS) {
		log.Warn("mqtt.tls.* is ignored because the broker URL is not ssl/tls/mqtts/wss",
			"broker", cfg.Broker)
	}

	cl.c = paho.NewClient(opts)
	go cl.pump()
	return cl, nil
}

func tlsFieldsSet(t config.TLSConfig) bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.InsecureSkipVerify
}

// handleConnect runs on paho's goroutine after every successful CONNACK. CleanSession is
// true so the broker forgot every subscription; put them back before telling the app.
func (cl *Client) handleConnect(c paho.Client) {
	cl.log.Info("mqtt connected", "reconnects", cl.reconnects.Load())

	cl.mu.Lock()
	filters := make([]string, 0, len(cl.subs))
	for f := range cl.subs {
		filters = append(filters, f)
	}
	cl.mu.Unlock()

	for _, f := range filters {
		cl.subscribeNow(c, f)
	}
	if cl.onConnect != nil {
		cl.onConnect()
	}
}

func (cl *Client) handleLost(_ paho.Client, err error) {
	n := cl.reconnects.Add(1)
	cl.log.Warn("mqtt connection lost", "error", err, "reconnects", n)
	if cl.onLost != nil {
		cl.onLost(err)
	}
}

// subscribeNow sends one SUBSCRIBE and waits for the SUBACK. Failures are logged only;
// the next reconnect tries again.
func (cl *Client) subscribeNow(c paho.Client, filter string) {
	tok := c.Subscribe(filter, cl.qos, cl.route)
	if !tok.WaitTimeout(subscribeTimeout) {
		cl.log.Warn("mqtt subscribe timed out", "filter", filter)
		return
	}
	if err := tok.Error(); err != nil {
		cl.log.Warn("mqtt subscribe failed", "filter", filter, "error", err)
		return
	}
	cl.log.Debug("mqtt subscribed", "filter", filter)
}

// route is the single paho message handler. It looks the handler up by the filter the
// message matched and calls it inline, so handlers must be fast.
func (cl *Client) route(_ paho.Client, msg paho.Message) {
	topic := msg.Topic()
	cl.mu.Lock()
	var hs []Handler
	for f, h := range cl.subs {
		if topicMatches(f, topic) {
			hs = append(hs, h)
		}
	}
	cl.mu.Unlock()
	for _, h := range hs {
		h(topic, msg.Payload(), msg.Retained())
	}
}

// Connect kicks off the first connection and returns right away. With ConnectRetry on,
// paho keeps trying in the background until the broker answers.
func (cl *Client) Connect() error {
	cl.c.Connect()
	return nil
}

// Publish queues a message and returns at once. The pump drops messages while
// disconnected on purpose: the app keeps a last-payload cache and republishes
// on reconnect. A full queue (dead link for a long time) also drops, with a warning.
func (cl *Client) Publish(topic string, payload []byte, retained bool) {
	cl.enqueue(outMsg{topic: topic, payload: payload, retained: retained, qos: cl.qos})
}

func (cl *Client) enqueue(m outMsg) bool {
	select {
	case <-cl.stop:
		return false
	default:
	}
	select {
	case cl.out <- m:
		return true
	default:
		cl.log.Warn("mqtt publish queue full, dropping", "topic", m.topic)
		return false
	}
}

// pump is the single publisher goroutine. It keeps FIFO order (discovery,
// states, availability) and is the only place paho's Publish is called, so a
// stalled broker link blocks this goroutine and nothing else.
func (cl *Client) pump() {
	defer close(cl.pumpDone)
	for {
		select {
		case m := <-cl.out:
			cl.send(m)
		case <-cl.stop:
			return
		}
	}
}

func (cl *Client) send(m outMsg) {
	if m.done != nil {
		defer close(m.done)
	}
	if !cl.c.IsConnectionOpen() {
		cl.log.Debug("mqtt publish skipped, not connected", "topic", m.topic)
		return
	}
	tok := cl.c.Publish(m.topic, m.qos, m.retained, m.payload)
	wait := func() {
		if !tok.WaitTimeout(publishTimeout) {
			cl.log.Warn("mqtt publish timed out", "topic", m.topic)
			return
		}
		if err := tok.Error(); err != nil {
			cl.log.Warn("mqtt publish failed", "topic", m.topic, "error", err)
		}
	}
	if m.done != nil {
		// Close wants to know the message got out before the DISCONNECT.
		wait()
		return
	}
	go wait()
}

// Subscribe records a filter so it survives reconnects and subscribes right away when
// already connected. Subscribing the same filter twice replaces the handler.
func (cl *Client) Subscribe(filter string, h func(topic string, payload []byte, retained bool)) {
	cl.mu.Lock()
	cl.subs[filter] = h
	cl.mu.Unlock()
	if cl.c.IsConnectionOpen() {
		go cl.subscribeNow(cl.c, filter)
	}
}

// IsConnected reports whether the broker session is actually open right now.
// Paho's own IsConnected also says yes while it is merely retrying, which is
// not what callers gating publishes or --clear-discovery want.
func (cl *Client) IsConnected() bool { return cl.c.IsConnectionOpen() }

// Reconnects counts how many times the connection was lost since start.
func (cl *Client) Reconnects() int64 { return cl.reconnects.Load() }

// Close publishes a retained "offline" on availabilityTopic with a bounded wait, then
// disconnects cleanly. A clean DISCONNECT makes the broker drop the LWT, which is why
// the offline message is sent by hand first. The offline publish goes through the
// same queue so everything published before it still gets out first, and the whole
// of Close is bounded no matter what state paho is in: if the link is dead the LWT
// says offline anyway.
func (cl *Client) Close(availabilityTopic string) {
	cl.closeOnce.Do(func() {
		if cl.c.IsConnectionOpen() {
			if availabilityTopic != "" {
				done := make(chan struct{})
				if cl.enqueue(outMsg{topic: availabilityTopic, payload: []byte("offline"), retained: true, qos: 1, done: done}) {
					select {
					case <-done:
					case <-time.After(offlineTimeout):
						cl.log.Warn("mqtt offline publish timed out", "topic", availabilityTopic)
					}
				}
			}
			close(cl.stop)
			cl.c.Disconnect(disconnectQuiesceMs)
			cl.log.Info("mqtt disconnected")
			return
		}
		// Not connected, but the reconnect loop may still be running. Disconnect stops it.
		close(cl.stop)
		cl.c.Disconnect(0)
	})
}

// topicMatches implements MQTT filter matching with + and # wildcards.
func topicMatches(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fi, ti := 0, 0
	for fi < len(filter) {
		fEnd := indexOrEnd(filter, fi)
		level := filter[fi:fEnd]
		if level == "#" {
			return true
		}
		if ti > len(topic) {
			return false
		}
		tEnd := indexOrEnd(topic, ti)
		if level != "+" && level != topic[ti:tEnd] {
			return false
		}
		fi = fEnd + 1
		ti = tEnd + 1
	}
	return ti > len(topic)
}

func indexOrEnd(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return len(s)
}

// String makes log lines readable when the client is printed.
func (cl *Client) String() string {
	return fmt.Sprintf("mqtt.Client{connected=%v reconnects=%d}", cl.c.IsConnectionOpen(), cl.reconnects.Load())
}

var pahoLogOnce sync.Once

// pahoLogger adapts paho's Logger interface to slog at a fixed level.
type pahoLogger struct {
	log   *slog.Logger
	level slog.Level
}

func (p pahoLogger) Println(v ...any) {
	p.log.Log(context.Background(), p.level, strings.TrimSpace(fmt.Sprintln(v...)), "component", "paho")
}

func (p pahoLogger) Printf(format string, v ...any) {
	p.log.Log(context.Background(), p.level, strings.TrimSpace(fmt.Sprintf(format, v...)), "component", "paho")
}
