package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"esim/internal/service"
	"esim/internal/sms"
	"esim/internal/store"
)

type API struct {
	App   *service.App
	Token string
}
type requestKey struct{}

func New(a *service.App) http.Handler {
	api := &API{App: a, Token: a.Config.Token}
	m := http.NewServeMux()
	m.HandleFunc("GET /health/live", api.live)
	m.HandleFunc("GET /health/ready", api.ready)
	m.HandleFunc("GET /api/v1/health", api.health)
	m.HandleFunc("GET /api/v1/devices", api.devices)
	m.HandleFunc("GET /api/v1/devices/{id}", api.device)
	m.HandleFunc("GET /api/v1/messages", api.messages)
	m.HandleFunc("GET /api/v1/messages/{id}", api.message)
	m.HandleFunc("POST /api/v1/messages", api.send)
	m.HandleFunc("GET /api/v1/forward-tasks", api.tasks)
	m.HandleFunc("POST /api/v1/forward-tasks/{id}/retry", api.retry)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [12]byte
		if _, e := rand.Read(b[:]); e != nil {
			http.Error(w, "request id unavailable", 503)
			return
		}
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestKey{}, id), 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if r.URL.Path != "/health/live" && r.URL.Path != "/health/ready" {
			expected := "Bearer " + api.Token
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				api.err(w, r, 401, "unauthorized", "需要有效访问令牌")
				return
			}
		}
		if _, pattern := m.Handler(r); pattern == "" {
			api.err(w, r, 404, "not_found", "接口不存在或不支持此方法")
			return
		}
		m.ServeHTTP(w, r)
	})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func (a *API) err(w http.ResponseWriter, r *http.Request, status int, code, description string) {
	write(w, status, map[string]any{"error": map[string]any{"code": code, "message": description, "request_id": r.Context().Value(requestKey{})}})
}
func (a *API) storeError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case errors.Is(e, store.ErrNotFound):
		a.err(w, r, 404, "not_found", "记录不存在")
	case errors.Is(e, store.ErrConflict):
		a.err(w, r, 409, "idempotency_conflict", "幂等键已用于其他参数")
	case errors.Is(e, store.ErrState):
		a.err(w, r, 409, "invalid_state", "当前状态不允许此操作")
	default:
		a.err(w, r, 503, "storage_unavailable", "存储暂不可用")
	}
}
func (a *API) live(w http.ResponseWriter, r *http.Request) {
	h := a.App.Health()
	code := 200
	if !h.Live {
		code = 503
	}
	write(w, code, map[string]bool{"live": h.Live})
}
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	h := a.App.Health()
	code := 200
	if !h.Ready {
		code = 503
	}
	write(w, code, map[string]bool{"ready": h.Ready})
}
func (a *API) health(w http.ResponseWriter, r *http.Request) { write(w, 200, a.App.Health()) }
func (a *API) devices(w http.ResponseWriter, r *http.Request) {
	write(w, 200, map[string]any{"items": a.App.Devices()})
}
func (a *API) device(w http.ResponseWriter, r *http.Request) {
	d, ok := a.App.Device(r.PathValue("id"))
	if !ok {
		a.storeError(w, r, store.ErrNotFound)
		return
	}
	write(w, 200, d)
}
func positive(s string) (int64, error) {
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil || n < 1 {
		return 0, errors.New("invalid id")
	}
	return n, nil
}
func pagination(r *http.Request) (int64, int, error) {
	q := r.URL.Query()
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > 200 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = n
	}
	cursor := int64(0)
	if s := q.Get("cursor"); s != "" {
		n, e := positive(s)
		if e != nil {
			return 0, 0, e
		}
		cursor = n
	}
	return cursor, limit, nil
}
func (a *API) messages(w http.ResponseWriter, r *http.Request) {
	cursor, limit, e := pagination(r)
	if e != nil {
		a.err(w, r, 400, "invalid_query", "分页参数无效")
		return
	}
	q := r.URL.Query()
	f := store.Filter{Device: q.Get("device_id"), ICCID: q.Get("iccid"), Direction: q.Get("direction"), Number: q.Get("number"), Status: q.Get("status"), Cursor: cursor, Limit: limit + 1}
	if f.Direction != "" && f.Direction != "inbound" && f.Direction != "outbound" {
		a.err(w, r, 400, "invalid_query", "direction 无效")
		return
	}
	for _, x := range []struct {
		key  string
		dest *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if s := q.Get(x.key); s != "" {
			*x.dest, e = time.Parse(time.RFC3339, s)
			if e != nil {
				a.err(w, r, 400, "invalid_query", "时间必须使用 RFC3339")
				return
			}
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		a.err(w, r, 400, "invalid_query", "时间范围无效")
		return
	}
	items, e := a.App.Store.Messages(r.Context(), f)
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	write(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *API) message(w http.ResponseWriter, r *http.Request) {
	id, e := positive(r.PathValue("id"))
	if e != nil {
		a.err(w, r, 400, "invalid_id", "记录标识无效")
		return
	}
	m, e := a.App.Store.Message(r.Context(), id)
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	tasks, e := a.App.Store.Tasks(r.Context(), id, "", "", 0, 200)
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	write(w, 200, map[string]any{"message": m, "forward_tasks": tasks})
}
func (a *API) send(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		a.err(w, r, 400, "invalid_idempotency_key", "必须提供 1–128 字节的 Idempotency-Key")
		return
	}
	if r.Header.Get("Content-Type") != "application/json" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json;") {
		a.err(w, r, 415, "unsupported_media_type", "请使用 application/json")
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
		Number   string `json:"number"`
		Body     string `json:"body"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&req); e != nil {
		a.err(w, r, 400, "invalid_request", "请求 JSON 无效或过大")
		return
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		a.err(w, r, 400, "invalid_request", "仅允许一个 JSON 对象")
		return
	}
	if _, _, e := sms.EncodeSubmit(req.Number, req.Body); e != nil {
		code := e.Error()
		if code != "message_too_long" && code != "unsupported_character" {
			code = "invalid_message"
		}
		a.err(w, r, 422, code, "号码、正文编码或单条短信长度不符合要求")
		return
	}
	id, e := a.App.Store.FindSend(r.Context(), req.DeviceID, req.Number, req.Body, key)
	if e == nil {
		a.sendResponse(w, r, id, false)
		return
	}
	if !errors.Is(e, store.ErrNotFound) {
		a.storeError(w, r, e)
		return
	}
	d, ok := a.App.Device(req.DeviceID)
	if !ok {
		a.storeError(w, r, store.ErrNotFound)
		return
	}
	if !a.App.Health().Ready || !d.Enabled || d.State != "ready" || d.ICCID == "" || time.Since(d.CheckedAt) > max(2*a.App.Config.StatusInterval.Value(), 2*time.Minute) {
		a.err(w, r, 503, "device_unavailable", "设备或 SIM 尚未就绪")
		return
	}
	id, created, e := a.App.Store.EnqueueSend(r.Context(), req.DeviceID, d.ICCID, req.Number, req.Body, key, time.Now().UTC())
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	a.sendResponse(w, r, id, created)
}
func (a *API) sendResponse(w http.ResponseWriter, r *http.Request, id int64, created bool) {
	m, e := a.App.Store.Message(r.Context(), id)
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	w.Header().Set("Location", "/api/v1/messages/"+strconv.FormatInt(id, 10))
	write(w, 202, map[string]any{"message_id": id, "status": m.Status, "created": created})
}
func (a *API) tasks(w http.ResponseWriter, r *http.Request) {
	cursor, limit, e := pagination(r)
	if e != nil {
		a.err(w, r, 400, "invalid_query", "分页参数无效")
		return
	}
	var messageID int64
	if s := r.URL.Query().Get("message_id"); s != "" {
		messageID, e = positive(s)
		if e != nil {
			a.err(w, r, 400, "invalid_query", "message_id 无效")
			return
		}
	}
	items, e := a.App.Store.Tasks(r.Context(), messageID, r.URL.Query().Get("status"), r.URL.Query().Get("target_id"), cursor, limit+1)
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	write(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	id, e := positive(r.PathValue("id"))
	if e != nil {
		a.err(w, r, 400, "invalid_id", "任务标识无效")
		return
	}
	n, e := a.App.Store.RetryForward(r.Context(), id, time.Now().UTC(), a.App.Config.ForwardTTL.Value())
	if e != nil {
		a.storeError(w, r, e)
		return
	}
	write(w, 202, map[string]any{"task_id": n, "parent_id": id})
}
