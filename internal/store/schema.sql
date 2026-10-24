CREATE TABLE sims(iccid TEXT PRIMARY KEY, initialized INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages(
 id INTEGER PRIMARY KEY AUTOINCREMENT, device_id TEXT NOT NULL, iccid TEXT NOT NULL,
 direction TEXT NOT NULL, number TEXT NOT NULL, body TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', smsc_time TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, historical INTEGER NOT NULL DEFAULT 0,
 targets TEXT NOT NULL DEFAULT '[]', idempotency_key TEXT UNIQUE, request_hash TEXT,
 notified INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX messages_filter ON messages(device_id,iccid,direction,id);
CREATE TABLE receipts(
 id INTEGER PRIMARY KEY AUTOINCREMENT, device_id TEXT NOT NULL, iccid TEXT NOT NULL,
 storage TEXT NOT NULL, slot INTEGER NOT NULL, pdu TEXT NOT NULL,
 message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 active INTEGER NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX active_receipt ON receipts(device_id,iccid,storage,slot) WHERE active=1;
CREATE TABLE assemblies(
 message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
 device_id TEXT NOT NULL, iccid TEXT NOT NULL, sender TEXT NOT NULL,
 reference INTEGER NOT NULL, total INTEGER NOT NULL, encoding TEXT NOT NULL, first_at INTEGER NOT NULL
);
CREATE INDEX assembly_lookup ON assemblies(device_id,iccid,sender,reference,total,encoding,first_at);
CREATE TABLE fragments(
 message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 part INTEGER NOT NULL, body TEXT NOT NULL, pdu TEXT NOT NULL,
 PRIMARY KEY(message_id,part)
);
CREATE TABLE forward_tasks(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 target_id TEXT NOT NULL, parent_id INTEGER REFERENCES forward_tasks(id) ON DELETE SET NULL,
 part INTEGER NOT NULL, parts INTEGER NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL,
 group_name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, next_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE INDEX forwards_due ON forward_tasks(status,next_at);
CREATE TABLE forward_attempts(
 id INTEGER PRIMARY KEY AUTOINCREMENT, task_id INTEGER NOT NULL REFERENCES forward_tasks(id) ON DELETE CASCADE,
 created_at INTEGER NOT NULL, result TEXT NOT NULL
);
PRAGMA user_version=1;
