package bark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"esim/internal/config"
	"esim/internal/model"
	"esim/internal/notification"
)

func TestJSONPushAndErrors(t *testing.T) {
	for _, code := range []int{200, 400, 403, 408, 429, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/push" || r.Method != "POST" || r.URL.RawQuery != "" {
					t.Error("unsafe request")
				}
				var v map[string]string
				if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
					t.Error(e)
				}
				if v["body"] != "正文 / ? #" || v["device_key"] != "secret" {
					t.Error("payload missing")
				}
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(code)
				w.Write([]byte(`{"code":200}`))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), Targets: map[string]config.Target{"phone": {URL: srv.URL, Key: "secret"}}}
			r := c.Push(context.Background(), model.ForwardTask{TargetID: "phone", Body: "正文 / ? #"})
			if r.Success != (code == 200) || r.Retry != (code == 408 || code == 429 || code == 500) {
				t.Fatalf("%#v", r)
			}
			if strings.Contains(r.Reason, "secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestMinimalPayloadAndExplicitKeyPath(t *testing.T) {
	m := model.Message{ID: 1, DeviceID: "sim-01", Number: "10086", Body: "验证码为 123456。", Status: "received", CreatedAt: time.Now()}
	notes, e := notification.Build(m, model.ForwardSpec{Group: "simd-sim-01", DeviceName: "Main Modem", SIMLabel: "Personal SIM"}, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"push", "key"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]string
				if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
					t.Fatal(e)
				}
				expected := "/push"
				if mode == "key" {
					expected = "/private-key"
					if _, ok := payload["device_key"]; ok {
						t.Error("duplicate key routing")
					}
				} else if payload["device_key"] != "private-key" {
					t.Error("missing routing key")
				}
				if r.URL.Path != expected || r.URL.RawQuery != "" {
					t.Error("incorrect endpoint")
				}
				for k, want := range map[string]string{"title": "New Message", "icon": notification.DefaultIconURL, "id": "simd-sim-01-message-1-part-1"} {
					if payload[k] != want {
						t.Errorf("field %s", k)
					}
				}
				if payload["subtitle"] == "" || payload["body"] != m.Body {
					t.Error("message fields missing or modified")
				}
				for _, key := range []string{"markdown", "copy", "action", "level", "sound", "isArchive", "group"} {
					if _, ok := payload[key]; ok {
						t.Errorf("unexpected display field %s", key)
					}
				}
				w.Write([]byte(`{"code":200}`))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), Targets: map[string]config.Target{"phone": {URL: srv.URL, Key: "private-key", PathMode: mode}}}
			if r := c.Push(context.Background(), model.ForwardTask{TargetID: "phone", Notification: notes[0]}); !r.Success {
				t.Fatal(r)
			}
		})
	}
}
func TestRetryAfterAndBounds(t *testing.T) {
	now := time.Now()
	for _, s := range []string{"-1", "garbage"} {
		if parseRetryAfter(s, now) != 0 {
			t.Fatal(s)
		}
	}
	if parseRetryAfter("999999999", now) != 24*time.Hour {
		t.Fatal("unbounded Retry-After")
	}
	if d := NextDelay(100, 0); d > 15*time.Minute || d < 12*time.Minute {
		t.Fatal(d)
	}
	if d := NextDelay(1, time.Hour); d != time.Hour {
		t.Fatal(d)
	}
}
func TestRejectApplicationFailureAndNoCredentialsInError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":400,"message":"secret error body"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Targets: map[string]config.Target{"phone": {URL: srv.URL, Key: "secret"}}}
	r := c.Push(context.Background(), model.ForwardTask{TargetID: "phone"})
	if r.Success || r.Retry || r.Reason != "bark_rejected" {
		t.Fatal(r)
	}
	if r = c.Push(context.Background(), model.ForwardTask{TargetID: "missing"}); r.Reason != "target_missing" {
		t.Fatal(r)
	}
}
