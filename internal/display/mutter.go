package display

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// D-Bus names for Mutter's display configuration object on the session bus.
const (
	BusName    = "org.gnome.Mutter.DisplayConfig"
	ObjectPath = dbus.ObjectPath("/org/gnome/Mutter/DisplayConfig")
	Iface      = BusName
	// PropertyName is the bare property name as it appears in PropertiesChanged.
	PropertyName = "PowerSaveMode"
	// Property is the interface-qualified property name for Get/Set.
	Property = Iface + "." + PropertyName

	propertiesIface   = "org.freedesktop.DBus.Properties"
	propertiesChanged = propertiesIface + ".PropertiesChanged"
)

// Controller talks to Mutter over its own private session bus connection so that
// closing it, or the bus going away, never affects anything else in the process.
type Controller struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	sig     chan *dbus.Signal
	changes chan Mode
	errs    chan error
	log     *slog.Logger

	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

var _ Interface = (*Controller)(nil)

// New connects to the session bus, subscribes to PropertiesChanged on the
// DisplayConfig object and starts the signal pump. It does not read the mode;
// call Get for that. The ctx only bounds the setup calls.
func New(ctx context.Context) (*Controller, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	if err := conn.AddMatchSignalContext(ctx,
		dbus.WithMatchInterface(propertiesIface),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchObjectPath(ObjectPath),
	); err != nil {
		conn.Close()
		return nil, fmt.Errorf("add match for %s: %w", propertiesChanged, err)
	}

	c := &Controller{
		conn:    conn,
		obj:     conn.Object(BusName, ObjectPath),
		sig:     make(chan *dbus.Signal, 16),
		changes: make(chan Mode, 4),
		errs:    make(chan error, 1),
		log:     slog.Default().With("component", "display"),
		done:    make(chan struct{}),
	}
	conn.Signal(c.sig)
	go c.pump()
	return c, nil
}

// DefaultDialer is New with the Interface return type, for app.Deps.
func DefaultDialer(ctx context.Context) (Interface, error) {
	return New(ctx)
}

// Get reads PowerSaveMode. A value outside -1..3 is reported as ModeUnknown with an error.
func (c *Controller) Get(ctx context.Context) (Mode, error) {
	var v dbus.Variant
	err := c.obj.CallWithContext(ctx, propertiesIface+".Get", 0, Iface, PropertyName).Store(&v)
	if err != nil {
		return ModeUnknown, fmt.Errorf("get %s: %w", Property, err)
	}
	return modeFromVariant(v)
}

// Set writes PowerSaveMode. It refuses while Mutter reports -1 (no outputs), is a
// no-op when the display is already in the requested mode, and re-reads the
// property after writing so the app sees the result even if the signal is lost.
func (c *Controller) Set(ctx context.Context, m Mode) error {
	if m < ModeOn || m > ModeOff {
		return fmt.Errorf("refusing to write display mode %d", int32(m))
	}
	cur, err := c.Get(ctx)
	if err != nil {
		return err
	}
	if cur == ModeUnknown {
		return ErrUnknownState
	}
	if cur == m {
		return nil
	}
	err = c.obj.CallWithContext(ctx, propertiesIface+".Set", 0, Iface, PropertyName, dbus.MakeVariant(int32(m))).Err
	if err != nil {
		return fmt.Errorf("set %s to %d: %w", Property, int32(m), err)
	}
	after, err := c.Get(ctx)
	if err != nil {
		c.log.Warn("display re-read after set failed", "error", err)
		return nil
	}
	if after != m {
		c.log.Warn("display mode after set differs from request", "requested", m.String(), "actual", after.String())
	}
	c.push(after)
	return nil
}

// Changes yields modes seen through PropertiesChanged (and after a successful Set).
func (c *Controller) Changes() <-chan Mode { return c.changes }

// Errors yields exactly one error when the private connection dies.
func (c *Controller) Errors() <-chan error { return c.errs }

// Close drops the signal subscription and closes the private connection.
// It does not push on Errors. Safe to call more than once.
func (c *Controller) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		// Do not RemoveSignal here: godbus only closes channels that are still
		// registered when the connection closes, so removing it first would
		// leave the pump ranging over c.sig forever and this wait would hang.
		c.conn.Close()
		<-c.done
	})
}

// pump forwards PowerSaveMode updates from the signal channel to Changes. godbus
// closes the channel when the connection goes down, which ends the loop.
func (c *Controller) pump() {
	defer close(c.done)
	for s := range c.sig {
		if s == nil || s.Name != propertiesChanged || len(s.Body) < 3 {
			continue
		}
		if iface, ok := s.Body[0].(string); !ok || iface != Iface {
			continue
		}
		if changed, ok := s.Body[1].(map[string]dbus.Variant); ok {
			if v, ok := changed[PropertyName]; ok {
				m, err := modeFromVariant(v)
				if err != nil {
					c.log.Warn("bad PowerSaveMode in PropertiesChanged", "error", err)
					continue
				}
				c.push(m)
				continue
			}
		}
		if invalidated, ok := s.Body[2].([]string); ok && contains(invalidated, PropertyName) {
			m, err := c.Get(context.Background())
			if err != nil {
				c.log.Warn("re-read after invalidation failed", "error", err)
				continue
			}
			c.push(m)
		}
	}
	if c.closed.Load() {
		return
	}
	select {
	case c.errs <- errors.New("session bus connection closed"):
	default:
	}
}

// push hands a mode to Changes without blocking; when the buffer is full the
// oldest value is dropped so the newest always gets through.
func (c *Controller) push(m Mode) {
	for {
		select {
		case c.changes <- m:
			return
		default:
		}
		select {
		case <-c.changes:
		default:
		}
	}
}

func modeFromVariant(v dbus.Variant) (Mode, error) {
	i, ok := v.Value().(int32)
	if !ok {
		return ModeUnknown, fmt.Errorf("%s is %T, want int32", Property, v.Value())
	}
	m := Mode(i)
	if m < ModeUnknown || m > ModeOff {
		return ModeUnknown, fmt.Errorf("%s out of range: %d", Property, i)
	}
	return m, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
