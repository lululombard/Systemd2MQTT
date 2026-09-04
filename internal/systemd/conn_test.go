package systemd

import (
	"errors"
	"fmt"
	"io"
	"testing"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

func TestUnitStatusPredicates(t *testing.T) {
	cases := []struct {
		active, load        string
		on, failed, missing bool
	}{
		{"active", "loaded", true, false, false},
		{"activating", "loaded", true, false, false},
		{"reloading", "loaded", true, false, false},
		{"deactivating", "loaded", false, false, false},
		{"inactive", "loaded", false, false, false},
		{"failed", "loaded", false, true, false},
		{"inactive", "not-found", false, false, true},
	}
	for _, c := range cases {
		u := UnitStatus{Name: "x.service", ActiveState: c.active, LoadState: c.load}
		if got := u.IsOn(); got != c.on {
			t.Errorf("%s/%s IsOn=%v want %v", c.active, c.load, got, c.on)
		}
		if got := u.IsFailed(); got != c.failed {
			t.Errorf("%s/%s IsFailed=%v want %v", c.active, c.load, got, c.failed)
		}
		if got := u.IsMissing(); got != c.missing {
			t.Errorf("%s/%s IsMissing=%v want %v", c.active, c.load, got, c.missing)
		}
	}
}

func TestIsDisconnectErr(t *testing.T) {
	yes := []error{
		dbus.ErrClosed,
		fmt.Errorf("wrapped: %w", dbus.ErrClosed),
		errors.New("read unix @->/run/user/1000/bus: use of closed network connection"),
		errors.New("write: broken pipe"),
		io.EOF,
		errors.New("connection closed by peer"),
	}
	for _, err := range yes {
		if !IsDisconnectErr(err) {
			t.Errorf("IsDisconnectErr(%v) = false, want true", err)
		}
	}
	no := []error{
		nil,
		errors.New("Unit foo.service not found."),
		dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"},
	}
	for _, err := range no {
		if IsDisconnectErr(err) {
			t.Errorf("IsDisconnectErr(%v) = true, want false", err)
		}
	}
}

func TestIsPolkitDenied(t *testing.T) {
	yes := []error{
		dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}},
		dbus.Error{Name: "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired"},
		fmt.Errorf("start: %w", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}),
		errors.New("Interactive authentication required."),
	}
	for _, err := range yes {
		if !IsPolkitDenied(err) {
			t.Errorf("IsPolkitDenied(%v) = false, want true", err)
		}
	}
	no := []error{
		nil,
		dbus.ErrClosed,
		dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"},
		errors.New("something else"),
	}
	for _, err := range no {
		if IsPolkitDenied(err) {
			t.Errorf("IsPolkitDenied(%v) = true, want false", err)
		}
	}
}

func TestToJobResult(t *testing.T) {
	cases := map[string]JobResult{
		"done":       JobDone,
		"failed":     JobFailed,
		"canceled":   JobCanceled,
		"dependency": JobDependency,
		"skipped":    JobSkipped,
		"timeout":    JobTimeout,
		"weird":      JobResult("weird"),
	}
	for in, want := range cases {
		if got := toJobResult(in); got != want {
			t.Errorf("toJobResult(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapStatuses(t *testing.T) {
	names := []string{"a.service", "gone.service", "b.service"}
	raw := []sd.UnitStatus{
		{Name: "b.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
		{Name: "a.service", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"},
	}
	got, err := mapStatuses(config.ScopeUser, names, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d statuses, want 3", len(got))
	}
	if got[0].Name != "a.service" || !got[0].IsMissing() || got[0].Scope != config.ScopeUser {
		t.Errorf("a.service: %+v", got[0])
	}
	if got[1].Name != "gone.service" || !got[1].IsMissing() || got[1].ActiveState != "inactive" {
		t.Errorf("gone.service should be synthesised as not-found: %+v", got[1])
	}
	if got[2].Name != "b.service" || !got[2].IsOn() || got[2].SubState != "running" || got[2].LoadState != "loaded" {
		t.Errorf("b.service: %+v", got[2])
	}
}

func TestMapStatusesAliases(t *testing.T) {
	// display-manager.service is an alias of gdm.service: systemd answers with
	// the primary Id, the caller must still get the name it asked for.
	names := []string{"display-manager.service", "b.service"}
	raw := []sd.UnitStatus{
		{Name: "gdm.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
		{Name: "b.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
	}
	aliases := map[string]string{"display-manager.service": "gdm.service"}
	got, err := mapStatuses(config.ScopeUser, names, aliases, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "display-manager.service" || !got[0].IsOn() || got[0].IsMissing() {
		t.Errorf("alias via map: %+v", got[0])
	}

	// Without the alias map the same answer is matched by position, since
	// systemd returns exactly one entry per requested name in request order.
	got, err = mapStatuses(config.ScopeUser, names, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "display-manager.service" || !got[0].IsOn() {
		t.Errorf("alias by position: %+v", got[0])
	}
	if got[1].Name != "b.service" || got[1].IsOn() {
		t.Errorf("b.service: %+v", got[1])
	}

	// Positional matching must not steal an entry that belongs to another asked name.
	names = []string{"gone.service", "b.service"}
	raw = []sd.UnitStatus{
		{Name: "b.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
		{Name: "gone.service", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"},
	}
	got, _ = mapStatuses(config.ScopeUser, names, nil, raw)
	if !got[0].IsMissing() || !got[1].IsOn() {
		t.Errorf("by-name must win over position: %+v", got)
	}
}

func TestIsAlreadySubscribed(t *testing.T) {
	if !isAlreadySubscribed(dbus.Error{Name: "org.freedesktop.systemd1.AlreadySubscribed"}) {
		t.Error("dbus error name not recognised")
	}
	if !isAlreadySubscribed(errors.New("Client is already subscribed.")) {
		t.Error("message not recognised")
	}
	if isAlreadySubscribed(nil) || isAlreadySubscribed(dbus.ErrClosed) {
		t.Error("false positive")
	}
}
