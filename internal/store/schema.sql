CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT
);

CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  session_id TEXT,
  kind TEXT,
  agent_id TEXT,
  size INTEGER,
  mtime INTEGER,
  "offset" INTEGER NOT NULL DEFAULT 0,
  sha256_prefix TEXT,
  first_seen TEXT,
  last_seen TEXT,
  deleted_at TEXT,
  next_retry_at TEXT NULL
);

CREATE TABLE IF NOT EXISTS records (
  id INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL REFERENCES files(id),
  line_no INTEGER NOT NULL,
  byte_off INTEGER NOT NULL,
  byte_len INTEGER NOT NULL,
  uuid TEXT,
  type TEXT,
  subtype TEXT,
  session_id TEXT,
  parent_uuid TEXT,
  ts TEXT,
  raw BLOB,
  superseded INTEGER DEFAULT 0,
  UNIQUE(file_id, line_no)
);
CREATE INDEX IF NOT EXISTS records_session_ts ON records(session_id, ts);
CREATE INDEX IF NOT EXISTS records_uuid ON records(uuid);

CREATE TABLE IF NOT EXISTS messages (
  record_id INTEGER PRIMARY KEY REFERENCES records(id),
  role TEXT,
  model TEXT,
  text TEXT,
  text_len INTEGER,
  has_thinking INTEGER,
  tool_names TEXT,
  cost_usd REAL,
  input_tokens INTEGER,
  output_tokens INTEGER
);

CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY,
  record_id INTEGER NOT NULL REFERENCES records(id),
  session_id TEXT,
  kind TEXT,
  role TEXT,
  ts TEXT,
  seq INTEGER,
  text TEXT NOT NULL,
  embedded_at TEXT NULL
);
CREATE INDEX IF NOT EXISTS chunks_record ON chunks(record_id);
CREATE INDEX IF NOT EXISTS chunks_session ON chunks(session_id);
CREATE INDEX IF NOT EXISTS chunks_pending ON chunks(id) WHERE embedded_at IS NULL;

-- chunk_vec is created by store.Migrate with the configured dimensions.

CREATE VIRTUAL TABLE IF NOT EXISTS chunk_fts USING fts5(
  text,
  content='chunks',
  content_rowid='id',
  tokenize='trigram'
);

CREATE VIRTUAL TABLE IF NOT EXISTS tool_fts USING fts5(
  text,
  record_id UNINDEXED,
  tokenize='trigram'
);

CREATE TABLE IF NOT EXISTS sessions (
  session_id TEXT PRIMARY KEY,
  project TEXT,
  cwd TEXT,
  git_branch TEXT,
  started TEXT,
  ended TEXT,
  title TEXT,
  custom_title TEXT,
  message_count INTEGER,
  kind TEXT
);

CREATE TABLE IF NOT EXISTS assets (
  id INTEGER PRIMARY KEY,
  profile TEXT,
  session_id TEXT,
  path TEXT NOT NULL UNIQUE,
  kind TEXT,
  size INTEGER,
  mtime INTEGER,
  mode INTEGER,
  sha256 TEXT,
  raw BLOB,
  first_seen TEXT,
  last_seen TEXT,
  deleted_at TEXT,
  failed_at TEXT,
  fail_reason TEXT,
  next_retry_at TEXT
);
CREATE INDEX IF NOT EXISTS assets_session ON assets(session_id);

CREATE VIRTUAL TABLE IF NOT EXISTS asset_fts USING fts5(
  text,
  asset_id UNINDEXED,
  tokenize='trigram'
);

CREATE TABLE IF NOT EXISTS file_changes (
  id INTEGER PRIMARY KEY,
  record_id INTEGER NOT NULL REFERENCES records(id),
  tool_use_id TEXT,
  result_record_id INTEGER NULL REFERENCES records(id),
  result_ok INTEGER NULL,
  session_id TEXT,
  profile TEXT,
  ts TEXT,
  tool TEXT,
  file_path TEXT NOT NULL,
  op TEXT,
  added_chars INTEGER,
  removed_chars INTEGER,
  excerpt TEXT
);
CREATE INDEX IF NOT EXISTS file_changes_record ON file_changes(record_id);
CREATE INDEX IF NOT EXISTS file_changes_session ON file_changes(session_id);
CREATE INDEX IF NOT EXISTS file_changes_tool_use ON file_changes(tool_use_id);

CREATE VIRTUAL TABLE IF NOT EXISTS file_fts USING fts5(
  file_path,
  change_id UNINDEXED,
  tokenize='trigram'
);

CREATE TABLE IF NOT EXISTS tool_outputs (
  record_id INTEGER NOT NULL REFERENCES records(id),
  idx INTEGER NOT NULL,
  tool_use_id TEXT,
  inline_bytes INTEGER,
  saved_path TEXT,
  asset_id INTEGER NULL REFERENCES assets(id),
  UNIQUE(record_id, idx)
);
CREATE INDEX IF NOT EXISTS tool_outputs_unresolved ON tool_outputs(saved_path) WHERE saved_path IS NOT NULL AND asset_id IS NULL;

CREATE TABLE IF NOT EXISTS summaries (
  id INTEGER PRIMARY KEY,
  record_id INTEGER NULL REFERENCES records(id),
  idx INTEGER NULL,
  asset_id INTEGER NULL REFERENCES assets(id),
  change_id INTEGER NULL REFERENCES file_changes(id),
  text TEXT,
  model TEXT,
  created_at TEXT,
  UNIQUE(record_id, idx),
  UNIQUE(asset_id)
);

INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '1');
