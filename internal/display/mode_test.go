package display

import (
	"context"
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestParseMode(t *testing.T) {
	good := []struct {
		in   string
		want Mode
	}{
		{"on", ModeOn}, {"ON", ModeOn}, {"On", ModeOn}, {"0", ModeOn}, {" on ", ModeOn},
		{"standby", ModeStandby}, {"STANDBY", ModeStandby}, {"1", ModeStandby},
		{"suspend", ModeSuspend}, {"Suspend", ModeSuspend}, {"2", ModeSuspend},
		{"off", ModeOff}, {"OFF", ModeOff}, {"3", ModeOff}, {"\toff\n", ModeOff},
	}
	for _, tc := range good {
		got, err := ParseMode(tc.in)
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	bad := []string{"", " ", "unknown", "-1", "4", "o n", "true", "ON/OFF", "dim"}
	for _, in := range bad {
		got, err := ParseMode(in)
		if err == nil {
			t.Errorf("ParseMode(%q) = %v, want error", in, got)
			continue
		}
		if got != ModeUnknown {
			t.Errorf("ParseMode(%q) returned %v with error, want ModeUnknown", in, got)
		}
	}
}

func TestModeString(t *testing.T) {
	cases := map[Mode]string{
		ModeOn:      "on",
		ModeStandby: "standby",
		ModeSuspend: "suspend",
		ModeOff:     "off",
		ModeUnknown: "unknown",
		Mode(42):    "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int32(m), got, want)
		}
	}
}

func TestModeValues(t *testing.T) {
	// The numbers are Mutter's, not ours; make sure nobody reorders the enum.
	if ModeUnknown != -1 || ModeOn != 0 || ModeStandby != 1 || ModeSuspend != 2 || ModeOff != 3 {
		t.Fatalf("mode constants no longer match PowerSaveMode: %d %d %d %d %d",
			ModeUnknown, ModeOn, ModeStandby, ModeSuspend, ModeOff)
	}
}

func TestSwitchState(t *testing.T) {
	if got := ModeOn.SwitchState(); got != "ON" {
		t.Errorf("ModeOn.SwitchState() = %q, want ON", got)
	}
	for _, m := range []Mode{ModeStandby, ModeSuspend, ModeOff} {
		if got := m.SwitchState(); got != "OFF" {
			t.Errorf("%v.SwitchState() = %q, want OFF", m, got)
		}
	}
}

func TestSelectOptionsOrder(t *testing.T) {
	want := []string{"on", "standby", "suspend", "off"}
	if len(SelectOptions) != len(want) {
		t.Fatalf("SelectOptions = %v, want %v", SelectOptions, want)
	}
	for i, opt := range SelectOptions {
		if opt != want[i] {
			t.Errorf("SelectOptions[%d] = %q, want %q", i, opt, want[i])
		}
		// Each option must round-trip through ParseMode to the mode with that index.
		m, err := ParseMode(opt)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", opt, err)
			continue
		}
		if int(m) != i {
			t.Errorf("ParseMode(%q) = %d, want index %d", opt, m, i)
		}
		if m.String() != opt {
			t.Errorf("Mode %d String() = %q, want %q", i, m.String(), opt)
		}
	}
}

func TestFakeController(t *testing.T) {
	ctx := context.Background()
	f := NewFake(ModeOn)
	var _ Interface = f

	if m, err := f.Get(ctx); err != nil || m != ModeOn {
		t.Fatalf("Get = %v, %v; want on, nil", m, err)
	}

	// Set to the current mode is a no-op but still recorded.
	if err := f.Set(ctx, ModeOn); err != nil {
		t.Fatalf("Set(on) while on: %v", err)
	}
	select {
	case m := <-f.Changes():
		t.Fatalf("no-op Set should not publish a change, got %v", m)
	default:
	}

	if err := f.Set(ctx, ModeOff); err != nil {
		t.Fatalf("Set(off): %v", err)
	}
	if m := f.Mode(); m != ModeOff {
		t.Fatalf("mode after Set(off) = %v", m)
	}
	select {
	case m := <-f.Changes():
		if m != ModeOff {
			t.Fatalf("Changes yielded %v, want off", m)
		}
	default:
		t.Fatal("Set(off) should echo on Changes")
	}

	// Injected Set error.
	boom := errors.New("boom")
	f.FailSet(boom)
	if err := f.Set(ctx, ModeOn); !errors.Is(err, boom) {
		t.Fatalf("Set with injected error = %v, want boom", err)
	}
	if m := f.Mode(); m != ModeOff {
		t.Fatalf("failed Set must not change mode, got %v", m)
	}
	f.FailSet(nil)

	// Unknown state refuses writes.
	f.SetMode(ModeUnknown)
	if err := f.Set(ctx, ModeOn); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("Set while unknown = %v, want ErrUnknownState", err)
	}

	// Injected Get error.
	f.SetMode(ModeOn)
	f.FailGet(boom)
	if _, err := f.Get(ctx); !errors.Is(err, boom) {
		t.Fatalf("Get with injected error = %v, want boom", err)
	}
	f.FailGet(nil)

	if got := f.Sets(); len(got) != 4 || got[0] != ModeOn || got[1] != ModeOff || got[2] != ModeOn || got[3] != ModeOn {
		t.Fatalf("Sets() = %v", got)
	}

	// Change queue keeps the newest values when overfilled.
	for i := 0; i < 10; i++ {
		f.PushChange(Mode(i % 4))
	}
	var last Mode = ModeUnknown
	n := 0
drain:
	for {
		select {
		case last = <-f.Changes():
			n++
		default:
			break drain
		}
	}
	if n != 4 || last != Mode(9%4) {
		t.Fatalf("drained %d changes ending in %v, want 4 ending in %v", n, last, Mode(9%4))
	}

	// Errors carries at most one value.
	f.PushError(boom)
	f.PushError(errors.New("second"))
	select {
	case err := <-f.Errors():
		if !errors.Is(err, boom) {
			t.Fatalf("Errors yielded %v, want boom", err)
		}
	default:
		t.Fatal("PushError did not deliver")
	}
	select {
	case err := <-f.Errors():
		t.Fatalf("Errors yielded a second value %v", err)
	default:
	}

	f.Close()
	if !f.Closed() {
		t.Fatal("Closed() false after Close")
	}
}

func TestModeFromVariantAndPush(t *testing.T) {
	// Exercise the pieces of the real controller that need no bus.
	c := &Controller{changes: make(chan Mode, 4)}
	for i := 0; i < 9; i++ {
		c.push(Mode(i % 4))
	}
	var last Mode
	n := 0
drain:
	for {
		select {
		case last = <-c.changes:
			n++
		default:
			break drain
		}
	}
	if n != 4 || last != Mode(8%4) {
		t.Fatalf("drained %d changes ending in %v, want 4 ending in %v", n, last, Mode(8%4))
	}

	if !contains([]string{"a", "PowerSaveMode"}, "PowerSaveMode") || contains(nil, "x") {
		t.Fatal("contains is wrong")
	}

	for i, want := range []Mode{ModeOn, ModeStandby, ModeSuspend, ModeOff} {
		m, err := modeFromVariant(dbus.MakeVariant(int32(i)))
		if err != nil || m != want {
			t.Errorf("modeFromVariant(%d) = %v, %v; want %v", i, m, err, want)
		}
	}
	if m, err := modeFromVariant(dbus.MakeVariant(int32(-1))); err != nil || m != ModeUnknown {
		t.Errorf("modeFromVariant(-1) = %v, %v; want unknown, nil", m, err)
	}
	if _, err := modeFromVariant(dbus.MakeVariant(int32(7))); err == nil {
		t.Error("modeFromVariant(7) should fail")
	}
	if _, err := modeFromVariant(dbus.MakeVariant(uint32(0))); err == nil {
		t.Error("modeFromVariant(uint32) should fail")
	}
	if _, err := modeFromVariant(dbus.MakeVariant("on")); err == nil {
		t.Error("modeFromVariant(string) should fail")
	}
}
