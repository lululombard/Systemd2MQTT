package systemd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// aliasLookupTimeout bounds the one-off alias query in Dial so a wedged manager
// cannot stall the supervisor's redial loop.
const aliasLookupTimeout = 10 * time.Second

// Conn is one live connection to a systemd manager. It implements Manager.
//
// Signals from the bus are reduced by a pump goroutine to a single coalesced
// wake hint. The pump never calls D-Bus, so a burst of PropertiesChanged
// signals can at worst overflow go-systemd's update channel, which then shows
// up on errCh and also becomes a wake hint. A wake means "something you watch
// may have changed, go query", nothing more.
type Conn struct {
	scope config.Scope
	c     *sd.Conn
	watch map[string]struct{}
	// aliases maps a configured name to the unit's primary Id when they differ
	// (display-manager.service is really gdm.service). systemd answers with
	// the primary Id, so without this an aliased unit would look not-found.
	aliases map[string]string
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	log     *slog.Logger

	closeOnce sync.Once
}

// Dial connects to the user or system manager, subscribes to its signals and
// starts the pump that watches the given unit names.
func Dial(ctx context.Context, scope config.Scope, watch []string) (*Conn, error) {
	var (
		c   *sd.Conn
		err error
	)
	switch scope {
	case config.ScopeUser:
		c, err = sd.NewUserConnectionContext(ctx)
	case config.ScopeSystem:
		c, err = sd.NewSystemConnectionContext(ctx)
	default:
		return nil, fmt.Errorf("systemd: unknown scope %q", scope)
	}
	if err != nil {
		return nil, fmt.Errorf("systemd: connect %s manager: %w", scope, err)
	}

	if err := c.Subscribe(); err != nil && !isAlreadySubscribed(err) {
		c.Close()
		return nil, fmt.Errorf("systemd: subscribe %s manager: %w", scope, err)
	}

	conn := &Conn{
		scope:   scope,
		c:       c,
		watch:   make(map[string]struct{}, len(watch)),
		aliases: map[string]string{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		log:     slog.Default().With("scope", string(scope)),
	}
	for _, n := range watch {
		conn.watch[n] = struct{}{}
	}
	conn.resolveAliases(ctx, watch)

	updCh := make(chan *sd.PropertiesUpdate, 64)
	errCh := make(chan error, 8)
	c.SetPropertiesSubscriber(updCh, errCh)
	go conn.pump(updCh, errCh)

	return conn, nil
}

// DefaultDialer is Dial with the Dialer signature the Supervisor and the app use.
func DefaultDialer(ctx context.Context, scope config.Scope, watch []string) (Manager, error) {
	c, err := Dial(ctx, scope, watch)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// resolveAliases asks systemd once for the primary Id of every watched name and
// records the ones that differ, so both Query and the pump recognise the unit
// under either name. ListUnitsByNames answers in request order, one entry per
// name. A failure here is not fatal: Query will hit the same error and the
// supervisor handles that.
func (c *Conn) resolveAliases(ctx context.Context, watch []string) {
	if len(watch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, aliasLookupTimeout)
	defer cancel()
	raw, err := c.c.ListUnitsByNamesContext(ctx, watch)
	if err != nil || len(raw) != len(watch) {
		c.log.Debug("alias lookup skipped", "err", err, "answers", len(raw), "asked", len(watch))
		return
	}
	for i, r := range raw {
		if r.Name == "" || r.Name == watch[i] {
			continue
		}
		c.log.Info("unit is an alias", "unit", watch[i], "primary", r.Name)
		c.aliases[watch[i]] = r.Name
		c.watch[r.Name] = struct{}{}
	}
}

// pump turns bus signals into wake hints. It must never call D-Bus: go-systemd's
// dispatcher does non-blocking writes into updCh, so anything slow here would
// drop updates. A dropped update is not lost either, it lands on errCh and
// still wakes the reader, which then queries the truth.
func (c *Conn) pump(updCh <-chan *sd.PropertiesUpdate, errCh <-chan error) {
	defer close(c.done)
	for {
		select {
		case u := <-updCh:
			if u == nil {
				continue
			}
			if _, ok := c.watch[u.UnitName]; ok {
				c.hint()
			}
		case err := <-errCh:
			// The only thing go-systemd ever sends here is "update channel full".
			// That is a hint to re-query, never a disconnect.
			c.log.Debug("properties update dropped", "err", err)
			c.hint()
		case <-c.stop:
			return
		}
	}
}

func (c *Conn) hint() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Conn) Scope() config.Scope { return c.scope }

// Wake yields a coalesced hint that a watched unit may have changed.
func (c *Conn) Wake() <-chan struct{} { return c.wake }

// Query asks the manager for the current state of the given units.
// ListUnitsByNames loads units that are not in memory, which is wanted: a
// garbage collected template instance would otherwise be invisible. Units
// that do not exist come back with LoadState "not-found".
func (c *Conn) Query(ctx context.Context, names []string) ([]UnitStatus, error) {
	if len(names) == 0 {
		return nil, nil
	}
	raw, err := c.c.ListUnitsByNamesContext(ctx, names)
	if err != nil {
		return nil, err
	}
	return mapStatuses(c.scope, names, c.aliases, raw)
}

// mapStatuses converts go-systemd's result into UnitStatus, in the order the
// names were asked for and under the names the caller used. systemd reports
// the primary Id, so an aliased unit is found through aliases, and when the
// answer has one entry per name (which is what systemd does) an unmatched
// entry is taken by position. A name nothing answers for is reported as
// not-found rather than silently dropped.
func mapStatuses(scope config.Scope, names []string, aliases map[string]string, raw []sd.UnitStatus) ([]UnitStatus, error) {
	byName := make(map[string]sd.UnitStatus, len(raw))
	for _, r := range raw {
		byName[r.Name] = r
	}
	asked := make(map[string]struct{}, len(names))
	for _, n := range names {
		asked[n] = struct{}{}
	}
	positional := len(raw) == len(names)
	out := make([]UnitStatus, 0, len(names))
	for i, n := range names {
		r, ok := byName[n]
		if !ok {
			if primary, isAlias := aliases[n]; isAlias {
				r, ok = byName[primary]
			}
		}
		if !ok && positional {
			// Same count, unknown name at this position: systemd answered
			// with a primary Id we did not know about yet.
			if _, known := asked[raw[i].Name]; !known {
				r, ok = raw[i], true
			}
		}
		if !ok {
			out = append(out, UnitStatus{Name: n, Scope: scope, ActiveState: "inactive", SubState: "dead", LoadState: "not-found"})
			continue
		}
		out = append(out, UnitStatus{
			Name:        n,
			Scope:       scope,
			ActiveState: r.ActiveState,
			SubState:    r.SubState,
			LoadState:   r.LoadState,
		})
	}
	return out, nil
}

func (c *Conn) Start(ctx context.Context, name, mode string) (JobResult, error) {
	return c.job(ctx, "start", name, mode, c.c.StartUnitContext)
}

func (c *Conn) Stop(ctx context.Context, name, mode string) (JobResult, error) {
	return c.job(ctx, "stop", name, mode, c.c.StopUnitContext)
}

func (c *Conn) Restart(ctx context.Context, name, mode string) (JobResult, error) {
	return c.job(ctx, "restart", name, mode, c.c.RestartUnitContext)
}

type jobFunc func(ctx context.Context, name, mode string, ch chan<- string) (int, error)

// job enqueues one job and waits for its JobRemoved result or the context.
// The result channel must be buffered: go-systemd writes to it from its
// dispatcher and a blocked write would stall every other job on the connection.
func (c *Conn) job(ctx context.Context, verb, name, mode string, call jobFunc) (JobResult, error) {
	ch := make(chan string, 1)
	id, err := call(ctx, name, mode, ch)
	if err != nil {
		return JobError, err
	}
	c.log.Debug("job queued", "verb", verb, "unit", name, "mode", mode, "job_id", id)
	select {
	case res := <-ch:
		return toJobResult(res), nil
	case <-ctx.Done():
		return JobLocalTimeout, ctx.Err()
	}
}

// toJobResult maps systemd's JobRemoved result string to a JobResult.
func toJobResult(s string) JobResult {
	switch s {
	case "done":
		return JobDone
	case "failed":
		return JobFailed
	case "canceled":
		return JobCanceled
	case "dependency":
		return JobDependency
	case "skipped":
		return JobSkipped
	case "timeout":
		return JobTimeout
	}
	return JobResult(s)
}

// Close stops the pump and closes the bus connection. Safe to call twice.
//
// There is no Unsubscribe call on purpose. go-systemd's Unsubscribe has no
// context and no timeout, and Close is called exactly when the manager stopped
// answering, so it could hang for minutes and keep the supervisor from
// redialing. systemd drops the subscription itself when the connection goes.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
		<-c.done
		c.c.Close()
	})
}

// isAlreadySubscribed matches systemd's answer to a second Subscribe on the same client.
func isAlreadySubscribed(err error) bool {
	if err == nil {
		return false
	}
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.freedesktop.systemd1.AlreadySubscribed" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "alreadysubscribed") || strings.Contains(msg, "already subscribed")
}
