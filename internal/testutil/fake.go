// Package testutil supplies deterministic hardware doubles for integration tests.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"esim/internal/bark"
	"esim/internal/config"
	"esim/internal/model"
)

const Deliver = "000405810180F6000862014221436523044F60597D"

type Modem struct {
	Mu          sync.Mutex
	ICCID       string
	Rows        map[int]string
	Notify      chan struct{}
	Closed      chan struct{}
	once        sync.Once
	SendUnknown bool
	Sent        atomic.Int32
	OnRead      func()
	Deleted     atomic.Int32
}

func NewModem(id int) *Modem {
	return &Modem{ICCID: fmt.Sprintf("8986000000000000%04d", id), Rows: map[int]string{}, Notify: make(chan struct{}, 1), Closed: make(chan struct{})}
}
func (m *Modem) Initialize(ctx context.Context) (model.Device, error) { return m.Status(ctx) }
func (m *Modem) Status(context.Context) (model.Device, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return model.Device{State: "ready", ICCID: m.ICCID, SIMState: "READY", Model: "ML307R", Registration: 1, Capabilities: model.Capabilities{SMS: true, PDU: true, LongReceive: "unverified"}, CheckedAt: time.Now().UTC()}, nil
}
func (m *Modem) SIMIdentity(context.Context) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.ICCID, nil
}
func (m *Modem) Scan(context.Context) ([]model.StoredSMS, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	out := []model.StoredSMS{}
	for i, p := range m.Rows {
		out = append(out, model.StoredSMS{Storage: "ME", Index: i, PDU: p})
	}
	return out, nil
}
func (m *Modem) Read(ctx context.Context, store string, index int) (string, error) {
	m.Mu.Lock()
	hook := m.OnRead
	m.OnRead = nil
	m.Mu.Unlock()
	if hook != nil {
		hook()
	}
	m.Mu.Lock()
	p, ok := m.Rows[index]
	m.Mu.Unlock()
	if !ok {
		return "", errors.New("slot absent")
	}
	return p, nil
}
func (m *Modem) Delete(ctx context.Context, store string, index int) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	delete(m.Rows, index)
	m.Deleted.Add(1)
	return nil
}
func (m *Modem) Send(context.Context, string, string) (bool, error) {
	m.Sent.Add(1)
	if m.SendUnknown {
		return true, errors.New("timeout")
	}
	return false, nil
}
func (m *Modem) Notifications() <-chan struct{} { return m.Notify }
func (m *Modem) Done() <-chan struct{}          { return m.Closed }
func (m *Modem) Close() error                   { m.once.Do(func() { close(m.Closed) }); return nil }
func (m *Modem) Receive(index int, pdu string) {
	m.Mu.Lock()
	m.Rows[index] = pdu
	m.Mu.Unlock()
	select {
	case m.Notify <- struct{}{}:
	default:
	}
}

type Pusher struct{ Calls atomic.Int32 }

func (p *Pusher) Push(context.Context, model.ForwardTask) bark.Result {
	p.Calls.Add(1)
	return bark.Result{Success: true}
}
func Config(database string, n int) config.Config {
	c := config.Config{Listen: "127.0.0.1:8080", Token: "test-token-with-32-bytes-of-entropy", Database: database, ScanInterval: config.Duration(time.Hour), StatusInterval: config.Duration(time.Hour), AssemblyWait: config.Duration(5 * time.Minute), SendTTL: config.Duration(5 * time.Minute), ForwardTTL: config.Duration(24 * time.Hour), BarkTimeout: config.Duration(time.Second), RetentionDays: 90}
	for i := 0; i < n; i++ {
		c.Devices = append(c.Devices, config.Device{ID: fmt.Sprintf("modem-%d", i), Name: fmt.Sprintf("Modem %d", i), SIMLabel: "test", Driver: "ml307r", Targets: []string{"phone"}})
	}
	return c
}
