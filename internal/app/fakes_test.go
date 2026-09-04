package app

import (
	"strings"
	"sync"
)

// fakeMQTT records publishes and subscriptions and lets a test push inbound
// messages through the registered handlers, the way paho would.
type fakeMQTT struct {
	mu         sync.Mutex
	pubs       []fakePub
	subs       map[string]func(topic string, payload []byte, retained bool)
	connected  bool
	closedWith string
	closes     int
	reconnects int64

	onConnect func()
	onLost    func(error)
}

type fakePub struct {
	topic    string
	payload  string
	retained bool
}

func newFakeMQTT() *fakeMQTT {
	return &fakeMQTT{subs: map[string]func(string, []byte, bool){}}
}

// factory matches MQTTFactory and wires the callbacks in.
func (f *fakeMQTT) factory(_ string, onConnect func(), onLost func(error)) (MQTTClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onConnect = onConnect
	f.onLost = onLost
	return f, nil
}

// Connect flips to connected and fires onConnect right away, like a broker
// that answers instantly.
func (f *fakeMQTT) Connect() error {
	f.mu.Lock()
	f.connected = true
	cb := f.onConnect
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

// Reconnect simulates a drop and a fresh connection.
func (f *fakeMQTT) Reconnect(err error) {
	f.mu.Lock()
	f.reconnects++
	lost, conn := f.onLost, f.onConnect
	f.mu.Unlock()
	if lost != nil {
		lost(err)
	}
	if conn != nil {
		conn()
	}
}

func (f *fakeMQTT) Publish(topic string, payload []byte, retained bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubs = append(f.pubs, fakePub{topic: topic, payload: string(payload), retained: retained})
}

func (f *fakeMQTT) Subscribe(filter string, h func(topic string, payload []byte, retained bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[filter] = h
}

func (f *fakeMQTT) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeMQTT) Reconnects() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconnects
}

func (f *fakeMQTT) Close(availabilityTopic string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedWith = availabilityTopic
	f.closes++
	f.connected = false
}

// Deliver pushes an inbound message to every matching subscription. It
// returns false when nothing was subscribed for the topic.
func (f *fakeMQTT) Deliver(topic, payload string, retained bool) bool {
	f.mu.Lock()
	var handlers []func(string, []byte, bool)
	for filter, h := range f.subs {
		if filterMatches(filter, topic) {
			handlers = append(handlers, h)
		}
	}
	f.mu.Unlock()
	for _, h := range handlers {
		h(topic, []byte(payload), retained)
	}
	return len(handlers) > 0
}

// Publishes returns a copy of every publish so far, in order.
func (f *fakeMQTT) Publishes() []fakePub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePub(nil), f.pubs...)
}

// PublishesTo returns the payloads published on one topic, in order.
func (f *fakeMQTT) PublishesTo(topic string) []string {
	var out []string
	for _, p := range f.Publishes() {
		if p.topic == topic {
			out = append(out, p.payload)
		}
	}
	return out
}

// Last returns the last payload on a topic and whether there was one.
func (f *fakeMQTT) Last(topic string) (string, bool) {
	ps := f.PublishesTo(topic)
	if len(ps) == 0 {
		return "", false
	}
	return ps[len(ps)-1], true
}

// Count is how many times a topic was published.
func (f *fakeMQTT) Count(topic string) int { return len(f.PublishesTo(topic)) }

// Subscriptions lists the registered filters.
func (f *fakeMQTT) Subscriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.subs))
	for k := range f.subs {
		out = append(out, k)
	}
	return out
}

// ClosedWith reports the availability topic passed to Close, and the count.
func (f *fakeMQTT) ClosedWith() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closedWith, f.closes
}

// filterMatches is a small MQTT filter matcher with + and # for the fake.
func filterMatches(filter, topic string) bool {
	fp := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, part := range fp {
		if part == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if part != "+" && part != tp[i] {
			return false
		}
	}
	return len(fp) == len(tp)
}
