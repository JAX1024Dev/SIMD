CREATE TABLE runtime_health(id INTEGER PRIMARY KEY CHECK(id=1), checked_at INTEGER NOT NULL);
INSERT INTO runtime_health VALUES(1,0);
PRAGMA user_version=2;
