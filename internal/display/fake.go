package display

import (
	"context"
	"sync"
)

// FakeController is an in-memory Interface for other packages' tests. Every
// method is safe to call from any goroutine.
type FakeController struct {
	mu      sync.Mutex
	mode    Mode
	getErr  error
	setErr  error
	sets    []Mode
	closed  bool
	changes chan Mode
	errs    chan error
}

var _ Interface = (*FakeController)(nil)

// NewFake returns a controller reporting the given mode.
func NewFake(mode Mode) *FakeController {
	return &FakeController{
		mode:    mode,
		changes: make(chan Mode, 4),
		errs:    make(chan error, 1),
	}
}

// SetMode changes what Get returns without touching Changes or the Set log.
func (f *FakeController) SetMode(m Mode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = m
}

// Mode returns what Get would return.
func (f *FakeController) Mode() Mode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

// FailGet makes Get return err (nil clears it).
func (f *FakeController) FailGet(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

// FailSet makes Set return err after recording the call (nil clears it).
func (f *FakeController) FailSet(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setErr = err
}

// Sets returns a copy of every mode passed to Set, in order, including calls
// that returned an error.
func (f *FakeController) Sets() []Mode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Mode(nil), f.sets...)
}

// Closed reports whether Close has been called.
func (f *FakeController) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// PushChange delivers m on Changes, dropping the oldest queued value if full.
func (f *FakeController) PushChange(m Mode) {
	for {
		select {
		case f.changes <- m:
			return
		default:
		}
		select {
		case <-f.changes:
		default:
		}
	}
}

// PushError delivers err on Errors. It does nothing if one is already queued,
// matching the real controller's "exactly one" behaviour.
func (f *FakeController) PushError(err error) {
	select {
	case f.errs <- err:
	default:
	}
}

func (f *FakeController) Get(ctx context.Context) (Mode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return ModeUnknown, f.getErr
	}
	return f.mode, nil
}

// Set mirrors the real controller: refuses on ModeUnknown, no-op when already
// there, otherwise records the call, applies the injected error, and on success
// updates the mode and echoes it on Changes.
func (f *FakeController) Set(ctx context.Context, m Mode) error {
	f.mu.Lock()
	f.sets = append(f.sets, m)
	if f.getErr != nil {
		f.mu.Unlock()
		return f.getErr
	}
	if f.mode == ModeUnknown {
		f.mu.Unlock()
		return ErrUnknownState
	}
	if f.setErr != nil {
		f.mu.Unlock()
		return f.setErr
	}
	if f.mode == m {
		f.mu.Unlock()
		return nil
	}
	f.mode = m
	f.mu.Unlock()
	f.PushChange(m)
	return nil
}

func (f *FakeController) Changes() <-chan Mode { return f.changes }

func (f *FakeController) Errors() <-chan error { return f.errs }

func (f *FakeController) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}
