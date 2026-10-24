package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"esim/internal/bark"
	"esim/internal/config"
	"esim/internal/model"
	"esim/internal/modem"
	"esim/internal/store"
	"esim/internal/testutil"
)

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

type selectivePusher struct {
	mu       sync.Mutex
	repaired bool
	calls    map[string]int
}

func (p *selectivePusher) Push(ctx context.Context, t model.ForwardTask) bark.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[t.TargetID]++
	if t.TargetID == "bad" && !p.repaired {
		return bark.Result{Reason: "bark_rejected"}
	}
	return bark.Result{Success: true}
}
func TestTargetsIndependentAndManualRecoveryClearsHealth(t *testing.T) {
	c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 1)
	c.Devices[0].Targets = []string{"ok", "bad"}
	s, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	a := New(c, s)
	m := testutil.NewModem(0)
	a.Connect = func(context.Context, config.Device) (modem.Driver, error) { return m, nil }
	p := &selectivePusher{calls: map[string]int{}}
	a.Pusher = p
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	defer func() { cancel(); a.Wait() }()
	eventually(t, func() bool { return a.Health().Ready })
	time.Sleep(30 * time.Millisecond)
	m.Receive(1, testutil.Deliver)
	var failedID int64
	eventually(t, func() bool {
		tasks, e := s.Tasks(ctx, 0, "", "", 0, 50)
		if e != nil || len(tasks) != 2 {
			return false
		}
		for _, task := range tasks {
			if task.TargetID == "bad" && task.Status == "failed" {
				failedID = task.ID
			}
		}
		return failedID > 0 && a.Health().Status == "degraded"
	})
	p.mu.Lock()
	p.repaired = true
	p.mu.Unlock()
	retryID, e := s.RetryForward(ctx, failedID, time.Now(), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		tasks, _ := s.Tasks(ctx, 0, "", "", 0, 50)
		for _, task := range tasks {
			if task.ID == retryID && task.Status == "succeeded" {
				return a.Health().Status == "healthy"
			}
		}
		return false
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls["ok"] != 1 || p.calls["bad"] != 2 {
		t.Fatalf("unexpected repeated targets %#v", p.calls)
	}
}
func TestDatabaseFailureNeverDeletesModemSMS(t *testing.T) {
	c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 1)
	s, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	a := New(c, s)
	s.Close()
	m := testutil.NewModem(0)
	m.Receive(1, testutil.Deliver)
	if e = a.scan(context.Background(), c.Devices[0], m); e == nil || m.Deleted.Load() != 0 {
		t.Fatal("deleted SMS without durable commit")
	}
}
func TestEightModemsIndependentWithOneOffline(t *testing.T) {
	c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 8)
	s, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	a := New(c, s)
	fake := map[string]*testutil.Modem{}
	for i := 0; i < 7; i++ {
		fake[fmt.Sprintf("modem-%d", i)] = testutil.NewModem(i)
	}
	a.Connect = func(ctx context.Context, d config.Device) (modem.Driver, error) {
		m, ok := fake[d.ID]
		if !ok {
			return nil, errors.New("unplugged")
		}
		return m, nil
	}
	p := &testutil.Pusher{}
	a.Pusher = p
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	defer func() { cancel(); a.Wait() }()
	eventually(t, func() bool {
		h := a.Health()
		if !h.Ready || h.Status != "degraded" {
			return false
		}
		for id := range fake {
			d, _ := a.Device(id)
			if d.State != "ready" {
				return false
			}
		}
		return true
	})
	// Wait for each first complete snapshot before injecting new messages.
	time.Sleep(50 * time.Millisecond)
	for _, m := range fake {
		m.Receive(1, testutil.Deliver)
	}
	eventually(t, func() bool { return p.Calls.Load() == 7 })
	messages, e := s.Messages(ctx, store.Filter{Limit: 20})
	if e != nil || len(messages) != 7 {
		t.Fatalf("%v messages=%d", e, len(messages))
	}
	for _, m := range messages {
		if m.Historical || m.ICCID == "" || m.Status != "received" {
			t.Fatalf("%#v", m)
		}
	}
}
func TestDeleteChecksSlotAndSIMAgain(t *testing.T) {
	for _, swapSIM := range []bool{false, true} {
		t.Run(fmt.Sprint(swapSIM), func(t *testing.T) {
			c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 1)
			s, e := store.Open(c.Database)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			a := New(c, s)
			m := testutil.NewModem(0)
			m.Rows[1] = testutil.Deliver
			m.OnRead = func() {
				m.Mu.Lock()
				defer m.Mu.Unlock()
				if swapSIM {
					m.ICCID = "89860000000000009999"
				} else {
					m.Rows[1] = "replacement"
				}
			}
			e = a.scan(context.Background(), c.Devices[0], m)
			if m.Deleted.Load() != 0 {
				t.Fatal("deleted changed slot or SIM")
			}
			if swapSIM && e == nil {
				t.Fatal("SIM change not detected")
			}
			if !swapSIM && e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestUnknownSendIsNotRetried(t *testing.T) {
	c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 1)
	s, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	a := New(c, s)
	m := testutil.NewModem(1)
	m.SendUnknown = true
	a.setDevice(c.Devices[0], model.Device{State: "ready", ICCID: m.ICCID}, "")
	id, _, e := s.EnqueueSend(context.Background(), c.Devices[0].ID, m.ICCID, "10086", "hi", "key", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if e = a.send(context.Background(), c.Devices[0], m); e != nil {
		t.Fatal(e)
	}
	a.send(context.Background(), c.Devices[0], m)
	got, _ := s.Message(context.Background(), id)
	if got.Status != "unknown" || m.Sent.Load() != 1 {
		t.Fatalf("%s %d", got.Status, m.Sent.Load())
	}
}
