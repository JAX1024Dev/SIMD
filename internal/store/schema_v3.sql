ALTER TABLE forward_tasks ADD COLUMN notification TEXT NOT NULL DEFAULT '{}';
PRAGMA user_version=3;
