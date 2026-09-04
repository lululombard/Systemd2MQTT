// Package display controls monitor power through Mutter's DisplayConfig on the session bus.
package display

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Mode mirrors org.gnome.Mutter.DisplayConfig.PowerSaveMode.
type Mode int32

const (
	ModeUnknown Mode = -1
	ModeOn      Mode = 0
	ModeStandby Mode = 1
	ModeSuspend Mode = 2
	ModeOff     Mode = 3
)

// ErrUnknownState is returned when Set is attempted while Mutter reports -1.
var ErrUnknownState = errors.New("display power state unknown (no outputs?)")

// SelectOptions are the Home Assistant select options, in PowerSaveMode order.
var SelectOptions = []string{"on", "standby", "suspend", "off"}

func (m Mode) String() string {
	switch m {
	case ModeOn:
		return "on"
	case ModeStandby:
		return "standby"
	case ModeSuspend:
		return "suspend"
	case ModeOff:
		return "off"
	default:
		return "unknown"
	}
}

// SwitchState is the ON/OFF payload for the display switch. Not meaningful for ModeUnknown.
func (m Mode) SwitchState() string {
	if m == ModeOn {
		return "ON"
	}
	return "OFF"
}

// ParseMode accepts the option names or the numbers 0..3, case-insensitively.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "0":
		return ModeOn, nil
	case "standby", "1":
		return ModeStandby, nil
	case "suspend", "2":
		return ModeSuspend, nil
	case "off", "3":
		return ModeOff, nil
	}
	return ModeUnknown, fmt.Errorf("unknown display mode %q", s)
}

// Interface is what the app uses; the Mutter controller implements it.
type Interface interface {
	Get(ctx context.Context) (Mode, error)
	Set(ctx context.Context, m Mode) error
	// Changes yields modes seen through PropertiesChanged.
	Changes() <-chan Mode
	// Errors yields exactly one error when the connection dies.
	Errors() <-chan error
	Close()
}

// Dialer connects a display controller.
type Dialer func(ctx context.Context) (Interface, error)
