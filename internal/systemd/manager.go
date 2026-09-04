// Package systemd talks to a systemd manager (user or system) over D-Bus.
package systemd

import (
	"context"
	"errors"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// UnitStatus is the authoritative state of one unit as reported by ListUnitsByNames.
type UnitStatus struct {
	Name        string
	Scope       config.Scope
	ActiveState string
	SubState    string
	LoadState   string
}

// IsOn is what the Home Assistant switch shows.
func (u UnitStatus) IsOn() bool {
	switch u.ActiveState {
	case "active", "activating", "reloading":
		return true
	}
	return false
}

func (u UnitStatus) IsFailed() bool  { return u.ActiveState == "failed" }
func (u UnitStatus) IsMissing() bool { return u.LoadState == "not-found" }

// JobResult is systemd's JobRemoved result, plus two synthetic values.
type JobResult string

const (
	JobDone         JobResult = "done"
	JobFailed       JobResult = "failed"
	JobCanceled     JobResult = "canceled"
	JobDependency   JobResult = "dependency"
	JobSkipped      JobResult = "skipped"
	JobTimeout      JobResult = "timeout"
	JobError        JobResult = "error"         // the D-Bus call itself failed
	JobLocalTimeout JobResult = "local-timeout" // job_timeout expired before JobRemoved
)

// ErrOffline is returned when no manager connection is currently available.
var ErrOffline = errors.New("systemd manager offline")

// Manager is one connected systemd manager. Signals are hints only; Query is the truth.
type Manager interface {
	Scope() config.Scope
	Query(ctx context.Context, names []string) ([]UnitStatus, error)
	Start(ctx context.Context, name, mode string) (JobResult, error)
	Stop(ctx context.Context, name, mode string) (JobResult, error)
	Restart(ctx context.Context, name, mode string) (JobResult, error)
	// Wake yields a coalesced hint that a watched unit may have changed.
	Wake() <-chan struct{}
	Close()
}

// Dialer connects a Manager for a scope, watching the given unit names.
type Dialer func(ctx context.Context, scope config.Scope, watch []string) (Manager, error)

// IsDisconnectErr reports whether err means the bus connection is gone.
func IsDisconnectErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, dbus.ErrClosed) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection closed", "use of closed network connection", "EOF", "broken pipe"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// IsPolkitDenied reports whether err is polkit refusing a manage-units call.
func IsPolkitDenied(err error) bool {
	if err == nil {
		return false
	}
	var de dbus.Error
	if errors.As(err, &de) {
		switch de.Name {
		case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
			return true
		}
	}
	return strings.Contains(err.Error(), "Interactive authentication required")
}
