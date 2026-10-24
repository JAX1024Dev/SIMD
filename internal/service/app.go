package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"esim/internal/at"
	"esim/internal/bark"
	"esim/internal/config"
	"esim/internal/model"
	"esim/internal/modem"
	"esim/internal/serial"
	"esim/internal/store"
)

var ErrUnavailable = errors.New("service_unavailable")

type Factory func(context.Context, config.Device) (modem.Driver, error)
type Pusher interface {
	Push(context.Context, model.ForwardTask) bark.Result
}
type App struct {
	Config    config.Config
	Store     *store.Store
	Connect   Factory
	Pusher    Pusher
	Log       *slog.Logger
	mu        sync.RWMutex
	devices   map[string]model.Device
	beats     map[string]time.Time
	dbOK      bool
	dbChecked time.Time
	summary   map[string]int
	wg        sync.WaitGroup
	fatal     chan error
}

func New(c config.Config, s *store.Store) *App {
	a := &App{Config: c, Store: s, Pusher: bark.New(c), Log: slog.Default(), devices: map[string]model.Device{}, beats: map[string]time.Time{}, summary: map[string]int{}, fatal: make(chan error, 1)}
	a.Connect = func(ctx context.Context, d config.Device) (modem.Driver, error) {
		p, e := serial.Open(d.Port, d.Baud, d.RTSCTS)
		if e != nil {
			return nil, e
		}
		return modem.NewML307R(at.New(p)), nil
	}
	for _, d := range c.Devices {
		state := "disabled"
		if d.IsEnabled() {
			state = "connecting"
		}
		a.devices[d.ID] = model.Device{ID: d.ID, Name: d.Name, SIMLabel: d.SIMLabel, Phone: d.Phone, Enabled: d.IsEnabled(), State: state, Signal: 99, CheckedAt: time.Now().UTC()}
	}
	return a
}
func (a *App) Start(ctx context.Context) {
	a.mu.Lock()
	now := time.Now()
	for _, id := range []string{"maintenance", "forward-0", "forward-1", "forward-2", "forward-3"} {
		a.beats[id] = now
	}
	a.mu.Unlock()
	a.launch(func() { a.maintenance(ctx) })
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("forward-%d", i)
		a.launch(func() { a.forward(ctx, id) })
	}
	for _, d := range a.Config.Devices {
		if d.IsEnabled() {
			a.launch(func() { a.device(ctx, d) })
		}
	}
}
func (a *App) launch(fn func())    { a.wg.Add(1); go func() { defer a.wg.Done(); fn() }() }
func (a *App) Wait()               { a.wg.Wait() }
func (a *App) Fatal() <-chan error { return a.fatal }
func (a *App) beat(id string)      { a.mu.Lock(); a.beats[id] = time.Now().UTC(); a.mu.Unlock() }
func (a *App) database(ok bool) {
	a.mu.Lock()
	a.dbOK = ok
	a.dbChecked = time.Now().UTC()
	a.mu.Unlock()
}
func (a *App) setDevice(c config.Device, d model.Device, reason string) {
	d.ID = c.ID
	d.Name = c.Name
	d.SIMLabel = c.SIMLabel
	d.Phone = c.Phone
	d.Enabled = c.IsEnabled()
	d.LastError = reason
	d.CheckedAt = time.Now().UTC()
	a.mu.Lock()
	a.devices[c.ID] = d
	a.mu.Unlock()
}
func (a *App) Devices() []model.Device {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := []model.Device{}
	for _, d := range a.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (a *App) Device(id string) (model.Device, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	d, ok := a.devices[id]
	return d, ok
}

type Health struct {
	Status           string               `json:"status"`
	Live             bool                 `json:"live"`
	Ready            bool                 `json:"ready"`
	DatabaseWritable bool                 `json:"database_writable"`
	DatabaseChecked  time.Time            `json:"database_checked_at"`
	Workers          map[string]time.Time `json:"worker_heartbeats"`
	ForwardTasks     map[string]int       `json:"forward_tasks"`
	Devices          []model.Device       `json:"devices"`
	CheckedAt        time.Time            `json:"checked_at"`
}

func (a *App) Health() Health {
	a.mu.RLock()
	h := Health{Status: "unhealthy", Live: true, DatabaseWritable: a.dbOK && time.Since(a.dbChecked) < 30*time.Second, DatabaseChecked: a.dbChecked, Workers: map[string]time.Time{}, ForwardTasks: map[string]int{}, Devices: []model.Device{}, CheckedAt: time.Now().UTC()}
	for k, v := range a.beats {
		h.Workers[k] = v
		if time.Since(v) > max(30*time.Second, 2*a.Config.BarkTimeout.Value()+5*time.Second) {
			h.Live = false
		}
	}
	if len(a.beats) != 5 {
		h.Live = false
	}
	for k, v := range a.summary {
		h.ForwardTasks[k] = v
	}
	ready, total := 0, 0
	storageFull := false
	for _, d := range a.devices {
		h.Devices = append(h.Devices, d)
		if d.Enabled {
			if d.Total > 0 && d.Used >= d.Total {
				storageFull = true
			}
			total++
			if d.State == "ready" && d.ICCID != "" && time.Since(d.CheckedAt) < max(2*a.Config.StatusInterval.Value(), 2*time.Minute) {
				ready++
			}
		}
	}
	a.mu.RUnlock()
	h.Ready = h.Live && h.DatabaseWritable && ready > 0
	if h.Ready {
		h.Status = "healthy"
		if ready < total || storageFull || h.ForwardTasks["retrying"] > 0 || h.ForwardTasks["unresolved"] > 0 {
			h.Status = "degraded"
		}
	}
	sort.Slice(h.Devices, func(i, j int) bool { return h.Devices[i].ID < h.Devices[j].ID })
	return h
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
func (a *App) device(ctx context.Context, c config.Device) {
	backoff := time.Second
	for ctx.Err() == nil {
		drv, e := a.Connect(ctx, c)
		if e != nil {
			reason := "serial_open_failed"
			if errors.Is(e, serial.ErrBusy) {
				reason = "serial_busy"
			}
			a.setDevice(c, model.Device{State: "offline"}, reason)
			if !pause(ctx, backoff) {
				return
			}
			backoff = min(60*time.Second, backoff*2)
			continue
		}
		d, e := drv.Initialize(ctx)
		if e != nil {
			if d.State == "" {
				d.State = "offline"
			}
			a.setDevice(c, d, "initialization_failed")
			drv.Close()
			if !pause(ctx, backoff) {
				return
			}
			backoff = min(60*time.Second, backoff*2)
			continue
		}
		backoff = time.Second
		e = a.connected(ctx, c, drv)
		drv.Close()
		if ctx.Err() != nil {
			return
		}
		d.ICCID = ""
		d.State = "offline"
		a.setDevice(c, d, "connection_lost")
		a.Log.Warn("modem reconnecting", "device_id", c.ID)
		if !pause(ctx, backoff) {
			return
		}
	}
}
func (a *App) connected(ctx context.Context, c config.Device, drv modem.Driver) error {
	if e := a.scan(ctx, c, drv); e != nil {
		return e
	}
	scan := time.NewTicker(a.Config.ScanInterval.Value())
	status := time.NewTicker(a.Config.StatusInterval.Value())
	send := time.NewTicker(time.Second)
	defer scan.Stop()
	defer status.Stop()
	defer send.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-drv.Done():
			return errors.New("modem disconnected")
		case <-scan.C:
			if e := a.scan(ctx, c, drv); e != nil {
				return e
			}
		case <-drv.Notifications():
			if e := a.scan(ctx, c, drv); e != nil {
				return e
			}
		case <-status.C:
			d, e := drv.Status(ctx)
			if e != nil {
				a.setDevice(c, d, "status_failed")
				return e
			}
			old, _ := a.Device(c.ID)
			a.setDevice(c, d, "")
			if old.ICCID != d.ICCID {
				return errors.New("SIM changed; reinitialize")
			}
		case <-send.C:
			if e := a.send(ctx, c, drv); e != nil {
				return e
			}
		}
	}
}
func (a *App) scan(ctx context.Context, c config.Device, drv modem.Driver) error {
	d, e := drv.Status(ctx)
	if e != nil {
		return e
	}
	raw, e := drv.Scan(ctx)
	if e != nil {
		return e
	}
	id, e := drv.SIMIdentity(ctx)
	if e != nil || id != d.ICCID {
		return errors.New("SIM changed during scan")
	}
	specs := []model.ForwardSpec{}
	for _, t := range c.Targets {
		specs = append(specs, model.ForwardSpec{TargetID: t, Group: "simd-" + c.ID})
	}
	receipts, e := a.Store.IngestSnapshot(ctx, c.ID, d.ICCID, raw, specs, time.Now().UTC(), a.Config.ForwardTTL.Value())
	if e != nil {
		a.database(false)
		return e
	}
	for _, r := range receipts {
		id, e = drv.SIMIdentity(ctx)
		if e != nil || id != d.ICCID {
			return errors.New("SIM changed before deletion")
		}
		pdu, err := drv.Read(ctx, r.Raw.Storage, r.Raw.Index)
		if err != nil {
			return err
		}
		if pdu != r.Raw.PDU {
			continue
		}
		id, e = drv.SIMIdentity(ctx)
		if e != nil || id != d.ICCID {
			return errors.New("SIM changed before deletion")
		}
		if e = drv.Delete(ctx, r.Raw.Storage, r.Raw.Index); e != nil {
			return e
		}
		if e = a.Store.ReceiptDeleted(ctx, r.ID); e != nil {
			a.database(false)
			return e
		}
	}
	a.setDevice(c, d, "")
	return nil
}
func (a *App) send(ctx context.Context, c config.Device, drv modem.Driver) error {
	pending, e := a.Store.HasQueuedSend(ctx, c.ID)
	if e != nil {
		a.database(false)
		return e
	}
	if !pending {
		return nil
	}
	// Refresh the physical SIM immediately before claiming and submitting work.
	d, e := drv.Status(ctx)
	if e != nil {
		return e
	}
	old, _ := a.Device(c.ID)
	if old.ICCID != "" && old.ICCID != d.ICCID {
		return errors.New("SIM changed; reinitialize")
	}
	a.setDevice(c, d, "")
	if d.State != "ready" {
		return nil
	}
	task, e := a.Store.ClaimSend(ctx, c.ID, d.ICCID, time.Now().UTC(), a.Config.SendTTL.Value())
	if e != nil {
		a.database(false)
		return e
	}
	if task == nil {
		return nil
	}
	state, reason := "submitted", ""
	id, e := drv.SIMIdentity(ctx)
	if e != nil {
		state, reason = "failed", "sim_identity_unavailable"
	} else if id != task.ICCID {
		state, reason = "failed", "sim_changed"
	} else {
		unknown, err := drv.Send(ctx, task.Number, task.Body)
		if err != nil {
			state, reason = "failed", "modem_rejected"
			if unknown {
				state, reason = "unknown", "submission_result_unknown"
			}
		}
	}
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = a.Store.FinishSend(finish, task.MessageID, state, reason); e != nil {
		a.database(false)
		return e
	}
	return nil
}
func (a *App) forward(ctx context.Context, id string) {
	for ctx.Err() == nil {
		a.beat(id)
		task, e := a.Store.ClaimForward(ctx, time.Now().UTC())
		if e != nil {
			a.database(false)
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		if task == nil {
			if !pause(ctx, 250*time.Millisecond) {
				return
			}
			continue
		}
		r := a.Pusher.Push(ctx, *task)
		now := time.Now().UTC()
		status, reason, next := "failed", r.Reason, now
		if r.Success {
			status, reason = "succeeded", ""
		} else if r.Retry {
			status = "pending"
			next = now.Add(bark.NextDelay(task.Attempts, r.RetryAfter))
			if !next.Before(task.ExpiresAt) {
				next = task.ExpiresAt
			}
			if !now.Before(task.ExpiresAt) {
				status = "expired"
			}
		}
		finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		e = a.Store.FinishForward(finish, task.ID, status, reason, next, now)
		cancel()
		if e != nil {
			a.database(false)
			a.Log.Error("forward result persistence failed", "task_id", task.ID)
			select {
			case a.fatal <- errors.New("forward result persistence failed"):
			default:
			}
			return
		}
	}
}
func (a *App) maintenance(ctx context.Context) {
	lastCleanup := time.Time{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		a.beat("maintenance")
		now := time.Now().UTC()
		op, cancel := context.WithTimeout(ctx, 10*time.Second)
		e := a.Store.Check(op)
		if e == nil {
			e = a.Store.ExpireSends(op, now, a.Config.SendTTL.Value())
		}
		if e == nil {
			e = a.Store.FlushAssemblies(op, now, a.Config.AssemblyWait.Value(), a.Config.ForwardTTL.Value())
		}
		if e == nil && now.Sub(lastCleanup) > 24*time.Hour {
			e = a.Store.Cleanup(op, now.Add(-time.Duration(a.Config.RetentionDays)*24*time.Hour))
			if e == nil {
				lastCleanup = now
			}
		}
		if e == nil {
			var counts map[string]int
			counts, e = a.Store.ForwardSummary(op)
			if e == nil {
				a.mu.Lock()
				a.summary = counts
				a.mu.Unlock()
			}
		}
		a.database(e == nil)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
