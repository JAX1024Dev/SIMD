package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esim/internal/config"
	"esim/internal/modem"
	"esim/internal/service"
	"esim/internal/store"
	"esim/internal/testutil"
)

func apiTest(t *testing.T) (*service.App, http.Handler) {
	t.Helper()
	c := testutil.Config(filepath.Join(t.TempDir(), "data", "simd.db"), 1)
	s, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	a := service.New(c, s)
	m := testutil.NewModem(0)
	a.Connect = func(context.Context, config.Device) (modem.Driver, error) { return m, nil }
	a.Pusher = &testutil.Pusher{}
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	t.Cleanup(func() { cancel(); a.Wait(); s.Close() })
	until := time.Now().Add(3 * time.Second)
	for !a.Health().Ready && time.Now().Before(until) {
		time.Sleep(time.Millisecond * 5)
	}
	if !a.Health().Ready {
		t.Fatal("not ready")
	}
	return a, New(a)
}
func req(h http.Handler, token, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndMinimalProbes(t *testing.T) {
	a, h := apiTest(t)
	for _, p := range []string{"/api/v1/messages", "/api/v1/devices", "/api/v1/health", "/unknown"} {
		w := req(h, "", "GET", p, "", "")
		if w.Code != 401 || !strings.Contains(w.Body.String(), "request_id") {
			t.Fatalf("%s %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/health/live", "/health/ready"} {
		w := req(h, "", "GET", p, "", "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "iccid") || strings.Contains(w.Body.String(), "devices") {
			t.Fatal(w.Body.String())
		}
	}
	w := req(h, a.Config.Token, "GET", "/api/v1/devices", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "modem-0") {
		t.Fatal(w.Body.String())
	}
}
func TestSendIdempotencyValidationAndQuery(t *testing.T) {
	a, h := apiTest(t)
	token := a.Config.Token
	body := `{"device_id":"modem-0","number":"10086","body":"你好"}`
	w := req(h, token, "POST", "/api/v1/messages", body, "key-1")
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var first map[string]any
	json.Unmarshal(w.Body.Bytes(), &first)
	w = req(h, token, "POST", "/api/v1/messages", body, "key-1")
	var second map[string]any
	json.Unmarshal(w.Body.Bytes(), &second)
	if w.Code != 202 || first["message_id"] != second["message_id"] || second["created"] != false {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = req(h, token, "POST", "/api/v1/messages", strings.Replace(body, "你好", "不同", 1), "key-1")
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, tc := range []struct {
		body, key string
		code      int
	}{{body, "", 400}, {body + " {}", "extra", 400}, {strings.Replace(body, "你好", strings.Repeat("你", 71), 1), "long", 422}, {strings.Replace(body, "你好", "😀", 1), "emoji", 422}, {`{"device_id":"missing","number":"10086","body":"hi"}`, "missing", 404}} {
		w = req(h, token, "POST", "/api/v1/messages", tc.body, tc.key)
		if w.Code != tc.code {
			t.Fatalf("want %d got %d %s", tc.code, w.Code, w.Body.String())
		}
	}
	w = req(h, token, "GET", "/api/v1/messages?limit=1&direction=outbound", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "你好") {
		t.Fatal(w.Body.String())
	}
	for _, q := range []string{"limit=201", "cursor=no", "from=not-time", "direction=sideways", "from=2026-10-25T00:00:00Z&to=2026-10-24T00:00:00Z"} {
		w = req(h, token, "GET", "/api/v1/messages?"+q, "", "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}
}
