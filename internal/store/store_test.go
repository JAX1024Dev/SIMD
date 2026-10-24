package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"esim/internal/model"
)

const pdu = "000405810180F6000862014221436523044F60597D"

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "simd.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}
func baseline(t *testing.T, s *Store) {
	t.Helper()
	if _, e := s.IngestSnapshot(context.Background(), "a", "sim", nil, nil, time.Now(), time.Hour); e != nil {
		t.Fatal(e)
	}
}
func targets() []model.ForwardSpec {
	return []model.ForwardSpec{{TargetID: "phone", Title: "A", Group: "a"}}
}
func TestBaselineAndCrashBeforeDelete(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	raw := []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}
	r, e := s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	m, e := s.Message(ctx, 1)
	if e != nil || !m.Historical {
		t.Fatalf("%v %#v", e, m)
	}
	tasks, _ := s.Tasks(ctx, 0, "", "", 0, 50)
	if len(tasks) != 0 {
		t.Fatal("historical SMS pushed")
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r2, e := s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour)
	if e != nil || r2[0].ID != r[0].ID {
		t.Fatalf("receipt duplicated %v", e)
	}
	items, _ := s.Messages(ctx, Filter{Limit: 50})
	if len(items) != 1 {
		t.Fatal("message duplicated")
	}
	if e = s.ReceiptDeleted(ctx, r[0].ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	items, _ = s.Messages(ctx, Filter{Limit: 50})
	tasks, _ = s.Tasks(ctx, 0, "", "", 0, 50)
	if len(items) != 2 || len(tasks) != 1 {
		t.Fatal("reused slot with equal text was suppressed")
	}
}
func TestSlotReplacementAndAtomicRollback(t *testing.T) {
	s, _ := openTest(t)
	baseline(t, s)
	ctx := context.Background()
	raw := []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}
	s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour)
	raw[0].PDU = "000405810180F6000862014221436523044F60597E"
	if _, e := s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour); e != nil {
		t.Fatal(e)
	}
	m, _ := s.Messages(ctx, Filter{Limit: 50})
	if len(m) != 2 {
		t.Fatal("slot reuse dropped SMS")
	}
	if _, e := s.db.Exec("CREATE TRIGGER fail_push BEFORE INSERT ON forward_tasks BEGIN SELECT RAISE(ABORT,'disk full simulation'); END"); e != nil {
		t.Fatal(e)
	}
	raw[0].Index = 2
	if _, e := s.IngestSnapshot(ctx, "a", "sim", raw, targets(), time.Now(), time.Hour); e == nil {
		t.Fatal("expected transaction failure")
	}
	m, _ = s.Messages(ctx, Filter{Limit: 50})
	if len(m) != 2 {
		t.Fatal("partial SMS commit without push task")
	}
}
func TestSendIdempotencySIMChangeAndRecovery(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	now := time.Now()
	id, created, e := s.EnqueueSend(ctx, "a", "old", "10086", "hi", "key", now)
	if e != nil || !created {
		t.Fatal(e)
	}
	id2, created, e := s.EnqueueSend(ctx, "a", "other", "10086", "hi", "key", now)
	if e != nil || created || id2 != id {
		t.Fatal("idempotency failed")
	}
	if _, _, e = s.EnqueueSend(ctx, "a", "old", "10086", "different", "key", now); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	task, e := s.ClaimSend(ctx, "a", "new", now, time.Minute)
	if e != nil || task != nil {
		t.Fatal(e)
	}
	m, _ := s.Message(ctx, id)
	if m.Status != "failed" || m.Error != "sim_changed" {
		t.Fatalf("%#v", m)
	}
	id, _, _ = s.EnqueueSend(ctx, "a", "new", "10086", "hi", "next", now)
	task, e = s.ClaimSend(ctx, "a", "new", now, time.Minute)
	if e != nil || task == nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	m, _ = s.Message(ctx, id)
	if m.Status != "unknown" {
		t.Fatal("interrupted sending was replayable")
	}
	task, _ = s.ClaimSend(ctx, "a", "new", now, time.Minute)
	if task != nil {
		t.Fatal("replayed unknown send")
	}
	id, _, _ = s.EnqueueSend(ctx, "a", "new", "10086", "hi", "expires", now.Add(-time.Hour))
	s.ExpireSends(ctx, now, time.Minute)
	m, _ = s.Message(ctx, id)
	if m.Status != "expired" {
		t.Fatal(m.Status)
	}
}
func TestMultipartOutOfOrderTimeoutAndLateCompletion(t *testing.T) {
	s, _ := openTest(t)
	baseline(t, s)
	ctx := context.Background()
	now := time.Now()
	part := func(index, num int, body string) model.StoredSMS {
		return model.StoredSMS{Storage: "ME", Index: index, PDU: fmt.Sprintf("004405810180F60008620142214365230A0500037A02%02X%s", num, body)}
	}
	r, e := s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{part(1, 2, "4E16754C")}, targets(), now, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	s.ReceiptDeleted(ctx, r[0].ID)
	s.FlushAssemblies(ctx, now.Add(6*time.Minute), 5*time.Minute, time.Hour)
	m, _ := s.Message(ctx, 1)
	if m.Status != "incomplete" {
		t.Fatal(m)
	}
	tasks, _ := s.Tasks(ctx, 0, "", "", 0, 50)
	if len(tasks) != 1 {
		t.Fatal("missing partial notification")
	}
	if _, e = s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{part(2, 1, "4F60597D")}, targets(), now.Add(7*time.Minute), time.Hour); e != nil {
		t.Fatal(e)
	}
	m, _ = s.Message(ctx, 1)
	if m.Body != "你好世界" || m.Status != "received" {
		t.Fatalf("%#v", m)
	}
	tasks, _ = s.Tasks(ctx, 0, "", "", 0, 50)
	if len(tasks) != 2 {
		t.Fatal("completion did not notify exactly once")
	}
	s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{part(2, 1, "4F60597D")}, targets(), now.Add(8*time.Minute), time.Hour)
	tasks, _ = s.Tasks(ctx, 0, "", "", 0, 50)
	if len(tasks) != 2 {
		t.Fatal("repeated completion notification")
	}
}
func TestForwardRecoveryRetryAndRetention(t *testing.T) {
	s, path := openTest(t)
	baseline(t, s)
	ctx := context.Background()
	now := time.Now()
	r, e := s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}, targets(), now.Add(-100*24*time.Hour), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	s.ReceiptDeleted(ctx, r[0].ID)
	if e = s.Cleanup(ctx, now.Add(-90*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Message(ctx, 1); e != nil {
		t.Fatal("pending task lost to retention")
	}
	task, e := s.ClaimForward(ctx, now.Add(-100*24*time.Hour+time.Minute))
	if e != nil || task == nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	task, e = s.ClaimForward(ctx, now)
	if e != nil || task != nil {
		t.Fatal("expired task claimed")
	}
	tasks, _ := s.Tasks(ctx, 0, "", "", 0, 50)
	if tasks[0].Status != "expired" {
		t.Fatal(tasks)
	}
	retry, e := s.RetryForward(ctx, tasks[0].ID, now, time.Hour)
	if e != nil || retry == tasks[0].ID {
		t.Fatal(e)
	}
	if e = s.Cleanup(ctx, now.Add(-90*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Message(ctx, 1); e != nil {
		t.Fatal("manual retry lost to retention")
	}
	task, e = s.ClaimForward(ctx, now)
	if e != nil || task == nil {
		t.Fatal(e)
	}
	s.FinishForward(ctx, task.ID, "succeeded", "", now, now)
	if e = s.Cleanup(ctx, now.Add(-90*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Message(ctx, 1); e != nil {
		t.Fatal("recent retry history lost to retention")
	}
	if e = s.Cleanup(ctx, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Message(ctx, 1); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}

func TestOfflineBackupRestore(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	baseline(t, s)
	now := time.Now()
	if _, e := s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}, targets(), now, time.Hour); e != nil {
		t.Fatal(e)
	}
	id, _, e := s.EnqueueSend(ctx, "a", "sim", "10086", "hi", "backup-key", now)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	dir := filepath.Join(t.TempDir(), "restored")
	if e = os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	files, e := os.ReadDir(filepath.Dir(path))
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range files {
		b, e := os.ReadFile(filepath.Join(filepath.Dir(path), f.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, f.Name()), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	restored, e := Open(filepath.Join(dir, "simd.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	m, e := restored.Message(ctx, id)
	if e != nil || m.Body != "hi" || m.Status != "queued" {
		t.Fatalf("%#v %v", m, e)
	}
	tasks, e := restored.Tasks(ctx, 0, "", "", 0, 50)
	if e != nil || len(tasks) != 1 || tasks[0].Status != "pending" {
		t.Fatal(tasks, e)
	}
}
func TestSingleProcessLock(t *testing.T) {
	s, path := openTest(t)
	if _, e := Open(path); e == nil {
		t.Fatal("second daemon acquired database")
	}
	s.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
}

func TestMigrationAndWriteHealth(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	if _, e := s.db.Exec("DROP TABLE runtime_health; ALTER TABLE forward_tasks DROP COLUMN notification; PRAGMA user_version=1"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 3 {
		t.Fatal("migration not applied")
	}
	if e = s.Check(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("PRAGMA query_only=ON"); e != nil {
		t.Fatal(e)
	}
	if e = s.Check(ctx); e == nil {
		t.Fatal("read-only database reported writable")
	}
	s.db.Exec("PRAGMA query_only=OFF")
}

func TestCorruptDatabaseIsNeverReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "simd.db")
	contents := []byte("not a SQLite database; preserve this evidence")
	os.WriteFile(path, contents, 0600)
	if s, e := Open(path); e == nil {
		s.Close()
		t.Fatal("accepted corrupt database")
	}
	after, e := os.ReadFile(path)
	if e != nil || string(after) != string(contents) {
		t.Fatal("corrupt database was overwritten")
	}
}

func TestNotificationPersistsAcrossRestartAndRetry(t *testing.T) {
	s, path := openTest(t)
	baseline(t, s)
	ctx := context.Background()
	now := time.Now()
	if _, e := s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}, targets(), now, time.Hour); e != nil {
		t.Fatal(e)
	}
	tasks, e := s.Tasks(ctx, 0, "", "", 0, 50)
	if e != nil || len(tasks) != 1 || tasks[0].Notification.Icon == "" || tasks[0].Notification.Title != "New Message" {
		t.Fatal("presentation not stored", e)
	}
	original := tasks[0].Notification
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	task, e := s.ClaimForward(ctx, now)
	if e != nil || task == nil || task.Notification != original {
		t.Fatal("presentation changed on restart", e)
	}
	s.FinishForward(ctx, task.ID, "failed", "http_400", now, now)
	id, e := s.RetryForward(ctx, task.ID, now, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	tasks, e = s.Tasks(ctx, 0, "", "", 0, 50)
	if e != nil || tasks[0].ID != id || tasks[0].Notification.ID == original.ID || tasks[0].Notification.Body != original.Body || tasks[0].Notification.Icon != original.Icon || tasks[0].Notification.Subtitle != original.Subtitle {
		t.Fatal("manual retry lost presentation or reused notification ID", e)
	}
}

func TestV2MigrationPreservesLegacyQueuedPayload(t *testing.T) {
	s, path := openTest(t)
	baseline(t, s)
	ctx := context.Background()
	s.IngestSnapshot(ctx, "a", "sim", []model.StoredSMS{{Storage: "ME", Index: 1, PDU: pdu}}, targets(), time.Now(), time.Hour)
	if _, e := s.db.Exec("ALTER TABLE forward_tasks DROP COLUMN notification; PRAGMA user_version=2"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	tasks, e := s.Tasks(ctx, 0, "", "", 0, 50)
	if e != nil || len(tasks) != 1 || tasks[0].Body == "" || tasks[0].Notification.Title != "" {
		t.Fatal("legacy payload lost in migration", e)
	}
}
