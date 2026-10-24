package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"esim/internal/config"
	"esim/internal/model"
)

type Result struct {
	Success, Retry bool
	Reason         string
	RetryAfter     time.Duration
}
type Client struct {
	HTTP    *http.Client
	Targets map[string]config.Target
}

func New(c config.Config) *Client {
	targets := map[string]config.Target{}
	for _, t := range c.BarkTargets {
		targets[t.ID] = t
	}
	return &Client{HTTP: &http.Client{Timeout: c.BarkTimeout.Value(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Targets: targets}
}
func (c *Client) Push(ctx context.Context, t model.ForwardTask) Result {
	target, ok := c.Targets[t.TargetID]
	if !ok {
		return Result{Reason: "target_missing"}
	}
	n := t.Notification
	if n.Title == "" {
		n = model.Notification{Title: t.Title, Body: t.Body, Group: t.Group}
	}
	endpoint := target.URL + "/push"
	key := target.Key
	if target.PathMode == "key" {
		endpoint = target.URL + "/" + url.PathEscape(target.Key)
		key = ""
	}
	b, e := json.Marshal(struct {
		model.Notification
		DeviceKey string `json:"device_key,omitempty"`
	}{n, key})
	if e != nil {
		return Result{Reason: "invalid_payload"}
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if e != nil {
		return Result{Reason: "invalid_target"}
	}
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return Result{Retry: true, Reason: "network_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return Result{Retry: true, Reason: "http_" + strconv.Itoa(resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	if resp.StatusCode != 200 {
		return Result{Reason: "http_" + strconv.Itoa(resp.StatusCode)}
	}
	var body struct {
		Code int `json:"code"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&body); e != nil {
		return Result{Retry: true, Reason: "invalid_response"}
	}
	if body.Code != 200 {
		return Result{Reason: "bark_rejected"}
	}
	return Result{Success: true}
}
func parseRetryAfter(s string, now time.Time) time.Duration {
	if n, e := strconv.Atoi(s); e == nil && n > 0 {
		if n > 86400 {
			n = 86400
		}
		return time.Duration(n) * time.Second
	}
	if t, e := http.ParseTime(s); e == nil && t.After(now) {
		return min(t.Sub(now), 24*time.Hour)
	}
	return 0
}
func NextDelay(attempt int, retryAfter time.Duration) time.Duration {
	n := max(0, min(attempt-1, 8))
	d := min(5*time.Second*time.Duration(1<<n), 15*time.Minute)
	d = time.Duration(float64(d) * (0.8 + rand.Float64()*0.4))
	d = min(d, 15*time.Minute)
	return max(d, retryAfter)
}
