package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"esim/internal/model"
	"esim/internal/notification"
	"esim/internal/sms"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed schema_v2.sql
var schemaV2 string

//go:embed schema_v3.sql
var schemaV3 string
var ErrNotFound = errors.New("not_found")
var ErrConflict = errors.New("idempotency_conflict")
var ErrState = errors.New("invalid_state")

type Store struct {
	db        *sql.DB
	lock      *os.File
	closeOnce sync.Once
}

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	info, e := os.Stat(dir)
	if e != nil {
		return nil, e
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("database directory must be private (chmod 700)")
	}
	lf, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		lf.Close()
		return nil, errors.New("database already owned by another daemon")
	}
	s := &Store{lock: lf}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return fail(e)
	}
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return fail(e)
	}
	f.Close()
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	for _, v := range []string{"busy_timeout(5000)", "foreign_keys(ON)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", v)
	}
	u.RawQuery = q.Encode()
	s.db, e = sql.Open("sqlite", u.String())
	if e != nil {
		return fail(e)
	}
	s.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var check string
	if e = s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); e != nil || check != "ok" {
		return fail(errors.New("database integrity check failed; restore a backup"))
	}
	var version int
	if e = s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil {
		return fail(e)
	}
	if version > 3 {
		return fail(errors.New("database schema is newer than daemon"))
	}
	if version == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fail(err)
		}
		if _, err = tx.ExecContext(ctx, schema); err != nil {
			tx.Rollback()
			return fail(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
		version = 1
	}
	if version == 1 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fail(err)
		}
		if _, err = tx.ExecContext(ctx, schemaV2); err != nil {
			tx.Rollback()
			return fail(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
		version = 2
	}
	if version == 2 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fail(err)
		}
		if _, err = tx.ExecContext(ctx, schemaV3); err != nil {
			tx.Rollback()
			return fail(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
	}
	// A process might have died after writing the modem body or completing a push.
	if _, e = s.db.ExecContext(ctx, "UPDATE messages SET status='unknown',error='process_interrupted' WHERE direction='outbound' AND status='sending'; UPDATE forward_tasks SET status='pending' WHERE status='running'"); e != nil {
		return fail(e)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if e = os.Chmod(path+suffix, 0600); e != nil && !os.IsNotExist(e) {
			return fail(e)
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	var e error
	s.closeOnce.Do(func() {
		if s.db != nil {
			e = s.db.Close()
		}
		if s.lock != nil {
			s.lock.Close()
		}
	})
	return e
}
func (s *Store) Check(ctx context.Context) error {
	_, e := s.db.ExecContext(ctx, "UPDATE runtime_health SET checked_at=? WHERE id=1", stamp(time.Now()))
	return e
}
func stamp(t time.Time) int64 { return t.UnixMilli() }
func when(n int64) time.Time  { return time.UnixMilli(n).UTC() }

type Receipt struct {
	ID  int64
	Raw model.StoredSMS
}

// IngestSnapshot atomically freezes a SIM's initial baseline and queues new work.
// All storage areas must have been successfully scanned before calling it.
func (s *Store) IngestSnapshot(ctx context.Context, device, iccid string, raw []model.StoredSMS, targets []model.ForwardSpec, now time.Time, forwardTTL time.Duration) ([]Receipt, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "INSERT INTO sims(iccid) VALUES(?) ON CONFLICT DO NOTHING", iccid); e != nil {
		return nil, e
	}
	var initialized bool
	if e = tx.QueryRowContext(ctx, "SELECT initialized FROM sims WHERE iccid=?", iccid).Scan(&initialized); e != nil {
		return nil, e
	}
	// Receipts absent from a complete snapshot are no longer occupying the slot.
	rows, e := tx.QueryContext(ctx, "SELECT id,storage,slot,pdu FROM receipts WHERE device_id=? AND iccid=? AND active=1", device, iccid)
	if e != nil {
		return nil, e
	}
	type prev struct {
		id    int64
		store string
		slot  int
		pdu   string
	}
	old := []prev{}
	for rows.Next() {
		var p prev
		if e = rows.Scan(&p.id, &p.store, &p.slot, &p.pdu); e != nil {
			rows.Close()
			return nil, e
		}
		old = append(old, p)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, e
	}
	rows.Close()
	present := map[string]string{}
	for _, r := range raw {
		present[fmt.Sprintf("%s:%d", r.Storage, r.Index)] = r.PDU
	}
	for _, p := range old {
		if present[fmt.Sprintf("%s:%d", p.store, p.slot)] != p.pdu {
			if _, e = tx.ExecContext(ctx, "UPDATE receipts SET active=0 WHERE id=?", p.id); e != nil {
				return nil, e
			}
		}
	}
	out := []Receipt{}
	for _, r := range raw {
		var receiptID int64
		e = tx.QueryRowContext(ctx, "SELECT id FROM receipts WHERE device_id=? AND iccid=? AND storage=? AND slot=? AND pdu=? AND active=1", device, iccid, r.Storage, r.Index, r.PDU).Scan(&receiptID)
		if e == nil {
			out = append(out, Receipt{receiptID, r})
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		d := sms.Decode(r.PDU)
		var id int64
		if d.Error == "" && d.ConcatTotal > 1 {
			id, e = findAssembly(ctx, tx, device, iccid, d, r.PDU, now)
			if e != nil {
				return nil, e
			}
		}
		if id == 0 {
			targetJSON, err := json.Marshal(targets)
			if err != nil {
				return nil, err
			}
			status := "received"
			if d.Error != "" {
				status = "decode_error"
			} else if d.ConcatTotal > 1 {
				status = "assembling"
			}
			res, err := tx.ExecContext(ctx, "INSERT INTO messages(device_id,iccid,direction,number,body,status,error,smsc_time,created_at,historical,targets) VALUES(?,?,'inbound',?,?,?,?,?,?,?,?)", device, iccid, d.Sender, d.Body, status, d.Error, d.SCTime, stamp(now), !initialized, string(targetJSON))
			if err != nil {
				return nil, err
			}
			id, _ = res.LastInsertId()
			if status == "assembling" {
				if _, e = tx.ExecContext(ctx, "INSERT INTO assemblies VALUES(?,?,?,?,?,?,?,?)", id, device, iccid, d.Sender, d.ConcatRef, d.ConcatTotal, d.Encoding, stamp(now)); e != nil {
					return nil, e
				}
			}
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO receipts(device_id,iccid,storage,slot,pdu,message_id) VALUES(?,?,?,?,?,?)", device, iccid, r.Storage, r.Index, r.PDU, id)
		if err != nil {
			return nil, err
		}
		receiptID, _ = res.LastInsertId()
		out = append(out, Receipt{receiptID, r})
		if d.Error == "" && d.ConcatTotal > 1 {
			if _, e = tx.ExecContext(ctx, "INSERT INTO fragments(message_id,part,body,pdu) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", id, d.ConcatPart, d.Body, r.PDU); e != nil {
				return nil, e
			}
			if e = refreshAssembly(ctx, tx, id, now, forwardTTL); e != nil {
				return nil, e
			}
		} else if e = queueMessage(ctx, tx, id, now, forwardTTL); e != nil {
			return nil, e
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE sims SET initialized=1 WHERE iccid=?", iccid); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return out, nil
}
func findAssembly(ctx context.Context, tx *sql.Tx, device, iccid string, d model.DecodedSMS, raw string, now time.Time) (int64, error) {
	rows, e := tx.QueryContext(ctx, `SELECT a.message_id,m.smsc_time,COALESCE(f.pdu,'') FROM assemblies a JOIN messages m ON m.id=a.message_id LEFT JOIN fragments f ON f.message_id=a.message_id AND f.part=? WHERE a.device_id=? AND a.iccid=? AND a.sender=? AND a.reference=? AND a.total=? AND a.encoding=? AND a.first_at>=? ORDER BY a.message_id DESC`, d.ConcatPart, device, iccid, d.Sender, d.ConcatRef, d.ConcatTotal, d.Encoding, stamp(now.Add(-24*time.Hour)))
	if e != nil {
		return 0, e
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ts, pdu string
		if e = rows.Scan(&id, &ts, &pdu); e != nil {
			return 0, e
		}
		a, ea := time.Parse(time.RFC3339, ts)
		b, eb := time.Parse(time.RFC3339, d.SCTime)
		if ea != nil || eb != nil || a.Sub(b) > 10*time.Minute || b.Sub(a) > 10*time.Minute {
			continue
		}
		if pdu != "" {
			if pdu == raw {
				return id, nil
			}
			continue
		}
		return id, nil
	}
	return 0, rows.Err()
}

func (s *Store) ReceiptDeleted(ctx context.Context, id int64) error {
	_, e := s.db.ExecContext(ctx, "UPDATE receipts SET active=0 WHERE id=?", id)
	return e
}
func queueMessage(ctx context.Context, tx *sql.Tx, id int64, now time.Time, ttl time.Duration) error {
	var body, status, sender, scts, targets string
	var historical bool
	var notified int
	var device string
	var created int64
	if e := tx.QueryRowContext(ctx, "SELECT body,status,number,smsc_time,targets,historical,notified,device_id,created_at FROM messages WHERE id=?", id).Scan(&body, &status, &sender, &scts, &targets, &historical, &notified, &device, &created); e != nil {
		return e
	}
	if historical || status == "assembling" || notified == 2 || notified == 1 && status == "incomplete" {
		return nil
	}
	var specs []model.ForwardSpec
	if e := json.Unmarshal([]byte(targets), &specs); e != nil {
		return e
	}
	if status == "decode_error" {
		body = ""
	}
	for _, spec := range specs {
		parts, e := notification.Build(model.Message{ID: id, DeviceID: device, Body: body, Number: sender, Status: status, SCTime: scts, CreatedAt: when(created)}, spec, notified == 1)
		if e != nil {
			return e
		}
		for i, p := range parts {
			encoded, e := json.Marshal(p)
			if e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, "INSERT INTO forward_tasks(message_id,target_id,part,parts,title,body,group_name,created_at,next_at,expires_at,notification) VALUES(?,?,?,?,?,?,?,?,?,?,?)", id, spec.TargetID, i+1, len(parts), p.Title, p.Body, spec.Group, stamp(now), stamp(now), stamp(now.Add(ttl)), string(encoded)); e != nil {
				return e
			}
		}
	}
	n := 2
	if status == "incomplete" {
		n = 1
	}
	_, e := tx.ExecContext(ctx, "UPDATE messages SET notified=? WHERE id=?", n, id)
	return e
}
func refreshAssembly(ctx context.Context, tx *sql.Tx, id int64, now time.Time, ttl time.Duration) error {
	var total int
	if e := tx.QueryRowContext(ctx, "SELECT total FROM assemblies WHERE message_id=?", id).Scan(&total); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, "SELECT part,body FROM fragments WHERE message_id=? ORDER BY part", id)
	if e != nil {
		return e
	}
	parts := map[int]string{}
	for rows.Next() {
		var p int
		var b string
		if e = rows.Scan(&p, &b); e != nil {
			rows.Close()
			return e
		}
		parts[p] = b
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	body := ""
	for i := 1; i <= total; i++ {
		if b, ok := parts[i]; ok {
			body += b
		} else {
			body += fmt.Sprintf("[Missing part %d/%d]", i, total)
		}
	}
	if len(parts) == total {
		_, e = tx.ExecContext(ctx, "UPDATE messages SET body=?,status='received' WHERE id=?", body, id)
	} else {
		_, e = tx.ExecContext(ctx, "UPDATE messages SET body=? WHERE id=?", body, id)
	}
	if e != nil {
		return e
	}
	return queueMessage(ctx, tx, id, now, ttl)
}
func (s *Store) FlushAssemblies(ctx context.Context, now time.Time, wait, ttl time.Duration) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT m.id FROM messages m JOIN assemblies a ON a.message_id=m.id WHERE m.status='assembling' AND a.first_at<=?", stamp(now.Add(-wait)))
	if e != nil {
		return e
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = tx.ExecContext(ctx, "UPDATE messages SET status='incomplete' WHERE id=?", id); e != nil {
			return e
		}
		if e = queueMessage(ctx, tx, id, now, ttl); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) EnqueueSend(ctx context.Context, device, iccid, number, body, key string, now time.Time) (int64, bool, error) {
	h := sha256.Sum256([]byte(device + "\x00" + number + "\x00" + body))
	hash := hex.EncodeToString(h[:])
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, false, e
	}
	defer tx.Rollback()
	var id int64
	var previous string
	e = tx.QueryRowContext(ctx, "SELECT id,request_hash FROM messages WHERE idempotency_key=?", key).Scan(&id, &previous)
	if e == nil {
		if hash != previous {
			return 0, false, ErrConflict
		}
		return id, false, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return 0, false, e
	}
	r, e := tx.ExecContext(ctx, "INSERT INTO messages(device_id,iccid,direction,number,body,status,created_at,idempotency_key,request_hash) VALUES(?,?,'outbound',?,?,'queued',?,?,?)", device, iccid, number, body, stamp(now), key, hash)
	if e != nil {
		return 0, false, e
	}
	id, _ = r.LastInsertId()
	return id, true, tx.Commit()
}
func (s *Store) FindSend(ctx context.Context, device, number, body, key string) (int64, error) {
	var id int64
	var hash string
	e := s.db.QueryRowContext(ctx, "SELECT id,request_hash FROM messages WHERE idempotency_key=?", key).Scan(&id, &hash)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if e != nil {
		return 0, e
	}
	h := sha256.Sum256([]byte(device + "\x00" + number + "\x00" + body))
	if hash != hex.EncodeToString(h[:]) {
		return 0, ErrConflict
	}
	return id, nil
}
func (s *Store) ClaimSend(ctx context.Context, device, iccid string, now time.Time, ttl time.Duration) (*model.SendTask, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE messages SET status='expired',error='queue_expired' WHERE direction='outbound' AND status='queued' AND device_id=? AND created_at<=?", device, stamp(now.Add(-ttl))); e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE messages SET status='failed',error='sim_changed' WHERE direction='outbound' AND status='queued' AND device_id=? AND iccid<>?", device, iccid); e != nil {
		return nil, e
	}
	t := &model.SendTask{}
	var created int64
	e = tx.QueryRowContext(ctx, "SELECT id,device_id,iccid,number,body,created_at FROM messages WHERE device_id=? AND status='queued' AND direction='outbound' ORDER BY id LIMIT 1", device).Scan(&t.MessageID, &t.DeviceID, &t.ICCID, &t.Number, &t.Body, &created)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if e != nil {
		return nil, e
	}
	t.CreatedAt = when(created)
	if _, e = tx.ExecContext(ctx, "UPDATE messages SET status='sending' WHERE id=?", t.MessageID); e != nil {
		return nil, e
	}
	return t, tx.Commit()
}
func (s *Store) FinishSend(ctx context.Context, id int64, status, reason string) error {
	_, e := s.db.ExecContext(ctx, "UPDATE messages SET status=?,error=? WHERE id=? AND status='sending'", status, reason, id)
	return e
}
func (s *Store) ExpireSends(ctx context.Context, now time.Time, ttl time.Duration) error {
	_, e := s.db.ExecContext(ctx, "UPDATE messages SET status='expired',error='queue_expired' WHERE direction='outbound' AND status='queued' AND created_at<=?", stamp(now.Add(-ttl)))
	return e
}

func (s *Store) HasQueuedSend(ctx context.Context, device string) (bool, error) {
	var exists bool
	e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages WHERE direction='outbound' AND device_id=? AND status='queued')", device).Scan(&exists)
	return exists, e
}

const taskCols = "id,message_id,target_id,parent_id,part,parts,title,body,group_name,status,attempts,last_error,created_at,next_at,expires_at,notification"

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (model.ForwardTask, error) {
	var t model.ForwardTask
	var a, b, c int64
	var presentation string
	e := row.Scan(&t.ID, &t.MessageID, &t.TargetID, &t.ParentID, &t.Part, &t.Parts, &t.Title, &t.Body, &t.Group, &t.Status, &t.Attempts, &t.LastError, &a, &b, &c, &presentation)
	if e == nil {
		e = json.Unmarshal([]byte(presentation), &t.Notification)
	}
	t.CreatedAt = when(a)
	t.NextAt = when(b)
	t.ExpiresAt = when(c)
	return t, e
}
func (s *Store) ClaimForward(ctx context.Context, now time.Time) (*model.ForwardTask, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE forward_tasks SET status='expired',last_error='retry_window_expired' WHERE status='pending' AND expires_at<=?", stamp(now)); e != nil {
		return nil, e
	}
	t, e := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskCols+" FROM forward_tasks WHERE status='pending' AND next_at<=? ORDER BY next_at,id LIMIT 1", stamp(now)))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if e != nil {
		return nil, e
	}
	t.Attempts++
	t.Status = "running"
	if _, e = tx.ExecContext(ctx, "UPDATE forward_tasks SET status='running',attempts=attempts+1 WHERE id=?", t.ID); e != nil {
		return nil, e
	}
	return &t, tx.Commit()
}
func (s *Store) FinishForward(ctx context.Context, id int64, status, reason string, next time.Time, now time.Time) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE forward_tasks SET status=?,last_error=?,next_at=? WHERE id=? AND status='running'", status, reason, stamp(next), id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO forward_attempts(task_id,created_at,result) VALUES(?,?,?)", id, stamp(now), status+":"+reason); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) RetryForward(ctx context.Context, id int64, now time.Time, ttl time.Duration) (int64, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	t, e := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskCols+" FROM forward_tasks WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if e != nil {
		return 0, e
	}
	if t.Status != "failed" && t.Status != "expired" {
		return 0, ErrState
	}
	r, e := tx.ExecContext(ctx, "INSERT INTO forward_tasks(message_id,target_id,parent_id,part,parts,title,body,group_name,created_at,next_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", t.MessageID, t.TargetID, id, t.Part, t.Parts, t.Title, t.Body, t.Group, stamp(now), stamp(now), stamp(now.Add(ttl)))
	if e != nil {
		return 0, e
	}
	n, _ := r.LastInsertId()
	if t.Notification.Title != "" {
		t.Notification.ID = fmt.Sprintf("%s-retry-%d", t.Notification.ID, n)
		encoded, err := json.Marshal(t.Notification)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE forward_tasks SET notification=? WHERE id=?", string(encoded), n); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}
func (s *Store) ForwardSummary(ctx context.Context) (map[string]int, error) {
	r, e := s.db.QueryContext(ctx, "SELECT status,count(*) FROM forward_tasks GROUP BY status")
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := map[string]int{}
	for r.Next() {
		var k string
		var n int
		if e = r.Scan(&k, &n); e != nil {
			return nil, e
		}
		out[k] = n
	}
	if e = r.Err(); e != nil {
		return nil, e
	}
	r.Close()
	var retrying int
	e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM forward_tasks WHERE status='pending' AND attempts>0").Scan(&retrying)
	out["retrying"] = retrying
	if e != nil {
		return nil, e
	}
	var unresolved int
	e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM forward_tasks f WHERE status IN ('failed','expired') AND NOT EXISTS(SELECT 1 FROM forward_tasks child WHERE child.parent_id=f.id)").Scan(&unresolved)
	out["unresolved"] = unresolved
	return out, e
}
func (s *Store) Cleanup(ctx context.Context, before time.Time) error {
	_, e := s.db.ExecContext(ctx, `DELETE FROM messages WHERE created_at<? AND status NOT IN ('queued','sending','assembling') AND NOT EXISTS(SELECT 1 FROM forward_tasks f WHERE f.message_id=messages.id AND (f.status IN ('pending','running') OR f.created_at>=?)) AND NOT EXISTS(SELECT 1 FROM forward_attempts a JOIN forward_tasks f ON f.id=a.task_id WHERE f.message_id=messages.id AND a.created_at>=?) AND NOT EXISTS(SELECT 1 FROM receipts r WHERE r.message_id=messages.id AND r.active=1)`, stamp(before), stamp(before), stamp(before))
	return e
}

const messageCols = "id,device_id,iccid,direction,number,body,status,error,smsc_time,created_at,historical"

func scanMessage(r scanner) (model.Message, error) {
	var m model.Message
	var n int64
	e := r.Scan(&m.ID, &m.DeviceID, &m.ICCID, &m.Direction, &m.Number, &m.Body, &m.Status, &m.Error, &m.SCTime, &n, &m.Historical)
	m.CreatedAt = when(n)
	return m, e
}
func (s *Store) Message(ctx context.Context, id int64) (model.Message, error) {
	m, e := scanMessage(s.db.QueryRowContext(ctx, "SELECT "+messageCols+" FROM messages WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if e != nil {
		return m, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT pdu FROM receipts WHERE message_id=? ORDER BY id", id)
	if e != nil {
		return m, e
	}
	defer rows.Close()
	m.Raw = []string{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			return m, e
		}
		m.Raw = append(m.Raw, p)
	}
	return m, rows.Err()
}

type Filter struct {
	Device, ICCID, Direction, Number, Status string
	From, To                                 time.Time
	Cursor                                   int64
	Limit                                    int
}

func (s *Store) Messages(ctx context.Context, f Filter) ([]model.Message, error) {
	w := []string{"1=1"}
	a := []any{}
	for _, v := range []struct{ col, val string }{{"device_id", f.Device}, {"iccid", f.ICCID}, {"direction", f.Direction}, {"number", f.Number}, {"status", f.Status}} {
		if v.val != "" {
			w = append(w, v.col+"=?")
			a = append(a, v.val)
		}
	}
	if !f.From.IsZero() {
		w = append(w, "created_at>=?")
		a = append(a, stamp(f.From))
	}
	if !f.To.IsZero() {
		w = append(w, "created_at<=?")
		a = append(a, stamp(f.To))
	}
	if f.Cursor > 0 {
		w = append(w, "id<?")
		a = append(a, f.Cursor)
	}
	a = append(a, f.Limit)
	rows, e := s.db.QueryContext(ctx, "SELECT "+messageCols+" FROM messages WHERE "+strings.Join(w, " AND ")+" ORDER BY id DESC LIMIT ?", a...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []model.Message{}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) Tasks(ctx context.Context, messageID int64, status, target string, cursor int64, limit int) ([]model.ForwardTask, error) {
	w := []string{"1=1"}
	a := []any{}
	if messageID > 0 {
		w = append(w, "message_id=?")
		a = append(a, messageID)
	}
	if status != "" {
		w = append(w, "status=?")
		a = append(a, status)
	}
	if target != "" {
		w = append(w, "target_id=?")
		a = append(a, target)
	}
	if cursor > 0 {
		w = append(w, "id<?")
		a = append(a, cursor)
	}
	a = append(a, limit)
	r, e := s.db.QueryContext(ctx, "SELECT "+taskCols+" FROM forward_tasks WHERE "+strings.Join(w, " AND ")+" ORDER BY id DESC LIMIT ?", a...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []model.ForwardTask{}
	for r.Next() {
		t, e := scanTask(r)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, r.Err()
}
