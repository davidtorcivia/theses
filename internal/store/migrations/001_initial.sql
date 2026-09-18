-- The whole schema from the plan's section 3. Single tenant, so nothing is keyed
-- by workspace. Times are unix seconds unless the column says otherwise.

CREATE TABLE users (
  id             INTEGER PRIMARY KEY,
  handle         TEXT    NOT NULL UNIQUE,
  email          TEXT    NOT NULL UNIQUE,
  name           TEXT    NOT NULL,
  initials       TEXT    NOT NULL,
  colour         TEXT    NOT NULL,
  role           TEXT    NOT NULL CHECK (role IN ('owner','editor','researcher','guest')),
  password_hash  TEXT    NOT NULL,
  totp_secret    TEXT    NOT NULL DEFAULT '',
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  session_epoch  INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  last_seen_at   INTEGER
);

CREATE TABLE sessions (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hmac       BLOB    NOT NULL UNIQUE,
  epoch      INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  user_agent TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE invitations (
  id          INTEGER PRIMARY KEY,
  email       TEXT    NOT NULL,
  role        TEXT    NOT NULL CHECK (role IN ('owner','editor','researcher','guest')),
  invited_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  token_hash  BLOB    NOT NULL UNIQUE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  accepted_at INTEGER
);

CREATE TABLE password_resets (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash BLOB    NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT    NOT NULL,
  secret     INTEGER NOT NULL DEFAULT 0,
  updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  hash         BLOB    NOT NULL UNIQUE,
  scopes       TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX api_tokens_user ON api_tokens(user_id);

CREATE TABLE propositions (
  id          INTEGER PRIMARY KEY,
  number      INTEGER NOT NULL UNIQUE,
  title       TEXT    NOT NULL,
  statement   TEXT    NOT NULL DEFAULT '',
  blurb       TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL,
  episode     TEXT,
  target_date TEXT,
  released_at INTEGER,
  duration    INTEGER,
  position    REAL    NOT NULL,
  created_at  INTEGER NOT NULL,
  archived_at INTEGER
);

CREATE TABLE proposition_members (
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_override  TEXT,
  PRIMARY KEY (proposition_id, user_id)
);

CREATE TABLE columns (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  position       REAL    NOT NULL
);
CREATE INDEX columns_proposition ON columns(proposition_id, position);

CREATE TABLE cards (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  column_id      INTEGER NOT NULL REFERENCES columns(id) ON DELETE CASCADE,
  position       REAL    NOT NULL,
  title          TEXT    NOT NULL,
  description_md TEXT    NOT NULL DEFAULT '',
  question       TEXT CHECK (question IN ('I','II','III','IV')),
  due_date       TEXT,
  done_at        INTEGER,
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at     INTEGER NOT NULL,
  version        INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX cards_column ON cards(column_id, position);

CREATE TABLE card_assignees (
  card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (card_id, user_id)
);

CREATE TABLE checklist_items (
  id       INTEGER PRIMARY KEY,
  card_id  INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  text     TEXT    NOT NULL,
  done     INTEGER NOT NULL DEFAULT 0,
  position REAL    NOT NULL
);
CREATE INDEX checklist_items_card ON checklist_items(card_id, position);

CREATE TABLE comments (
  id         INTEGER PRIMARY KEY,
  card_id    INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
  body_md    TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX comments_card ON comments(card_id, created_at);

CREATE TABLE documents (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  slug           TEXT    NOT NULL,
  position       REAL    NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at     INTEGER NOT NULL,
  UNIQUE (proposition_id, slug)
);

-- position is a fractional index (a sortable string) so inserts never renumber neighbours.
CREATE TABLE blocks (
  id          INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  position    TEXT    NOT NULL,
  text        TEXT    NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  updated_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER
);
CREATE INDEX blocks_document ON blocks(document_id, position);

CREATE TABLE document_revisions (
  id          INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  markdown    TEXT    NOT NULL,
  created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at  INTEGER NOT NULL,
  reason      TEXT    NOT NULL CHECK (reason IN ('manual','periodic','pre-import'))
);
CREATE INDEX document_revisions_document ON document_revisions(document_id, created_at);

CREATE TABLE links (
  id              INTEGER PRIMARY KEY,
  proposition_id  INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  url             TEXT    NOT NULL,
  canonical_url   TEXT    NOT NULL DEFAULT '',
  title           TEXT    NOT NULL DEFAULT '',
  author          TEXT    NOT NULL DEFAULT '',
  year            TEXT    NOT NULL DEFAULT '',
  kind            TEXT    NOT NULL DEFAULT '',
  note_md         TEXT    NOT NULL DEFAULT '',
  question        TEXT CHECK (question IN ('I','II','III','IV')),
  added_by        INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at      INTEGER NOT NULL,
  fetched_at      INTEGER,
  text_for_search TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX links_proposition ON links(proposition_id);

CREATE TABLE files (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  folder         TEXT    NOT NULL DEFAULT '',
  kind           TEXT    NOT NULL DEFAULT '',
  size           INTEGER NOT NULL DEFAULT 0,
  object_key     TEXT    NOT NULL,
  version_of     INTEGER REFERENCES files(id) ON DELETE SET NULL,
  duration_ms    INTEGER,
  width          INTEGER,
  height         INTEGER,
  uploaded_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
  state          TEXT    NOT NULL CHECK (state IN ('uploading','ready')),
  created_at     INTEGER NOT NULL
);
CREATE INDEX files_proposition ON files(proposition_id);

CREATE TABLE uploads (
  id           INTEGER PRIMARY KEY,
  file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  multipart_id TEXT    NOT NULL,
  parts_json   TEXT    NOT NULL DEFAULT '[]',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);

CREATE TABLE card_links (
  card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  link_id INTEGER NOT NULL REFERENCES links(id) ON DELETE CASCADE,
  PRIMARY KEY (card_id, link_id)
);

CREATE TABLE card_files (
  card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  PRIMARY KEY (card_id, file_id)
);

-- actor_id is not a foreign key: it names a user, an API token or an MCP client
-- depending on actor_kind, and the row outlives all three.
CREATE TABLE activity (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER REFERENCES propositions(id) ON DELETE CASCADE,
  actor_kind     TEXT    NOT NULL CHECK (actor_kind IN ('user','token','mcp','system')),
  actor_id       TEXT    NOT NULL DEFAULT '',
  entity         TEXT    NOT NULL,
  entity_id      TEXT    NOT NULL DEFAULT '',
  action         TEXT    NOT NULL,
  before_json    TEXT,
  after_json     TEXT,
  created_at     INTEGER NOT NULL,
  undone_at      INTEGER
);
CREATE INDEX activity_created ON activity(created_at);
CREATE INDEX activity_proposition ON activity(proposition_id, created_at);

CREATE TABLE notification_channels (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind        TEXT    NOT NULL CHECK (kind IN ('email','pushover','ntfy','webhook')),
  config_json TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,
  verified_at INTEGER
);
CREATE INDEX notification_channels_user ON notification_channels(user_id);

CREATE TABLE notification_rules (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  event      TEXT    NOT NULL,
  channel_id INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
  enabled    INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (user_id, event, channel_id)
);

CREATE TABLE notification_outbox (
  id           INTEGER PRIMARY KEY,
  channel_id   INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
  payload_json TEXT    NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  next_at      INTEGER NOT NULL,
  sent_at      INTEGER
);
CREATE INDEX notification_outbox_pending ON notification_outbox(sent_at, next_at);

CREATE TABLE mail_outbox (
  id         INTEGER PRIMARY KEY,
  to_addr    TEXT    NOT NULL,
  subject    TEXT    NOT NULL,
  body_text  TEXT    NOT NULL,
  body_html  TEXT    NOT NULL DEFAULT '',
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  next_at    INTEGER NOT NULL,
  sent_at    INTEGER,
  -- When the message stops being worth sending, because the link it carries
  -- dies: an hour for a password reset, a week for an invitation. NULL never
  -- expires.
  expires_at INTEGER,
  -- When delivery was first attempted. The day of retries runs from here, not
  -- from created_at, so a message queued before the workspace had an SMTP
  -- server still gets its full day once one exists. NULL means never tried.
  tried_at   INTEGER
);
CREATE INDEX mail_outbox_pending ON mail_outbox(sent_at, next_at);

-- Search. External-content FTS5 tables over the five searchable kinds, kept in
-- sync by triggers. 'delete' rows must carry the old values or the index rots.

CREATE VIRTUAL TABLE cards_fts USING fts5(
  title, description_md, content='cards', content_rowid='id'
);
CREATE TRIGGER cards_ai AFTER INSERT ON cards BEGIN
  INSERT INTO cards_fts(rowid, title, description_md) VALUES (new.id, new.title, new.description_md);
END;
CREATE TRIGGER cards_ad AFTER DELETE ON cards BEGIN
  INSERT INTO cards_fts(cards_fts, rowid, title, description_md) VALUES ('delete', old.id, old.title, old.description_md);
END;
CREATE TRIGGER cards_au AFTER UPDATE ON cards BEGIN
  INSERT INTO cards_fts(cards_fts, rowid, title, description_md) VALUES ('delete', old.id, old.title, old.description_md);
  INSERT INTO cards_fts(rowid, title, description_md) VALUES (new.id, new.title, new.description_md);
END;

CREATE VIRTUAL TABLE blocks_fts USING fts5(
  text, content='blocks', content_rowid='id'
);
CREATE TRIGGER blocks_ai AFTER INSERT ON blocks BEGIN
  INSERT INTO blocks_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER blocks_ad AFTER DELETE ON blocks BEGIN
  INSERT INTO blocks_fts(blocks_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
CREATE TRIGGER blocks_au AFTER UPDATE ON blocks BEGIN
  INSERT INTO blocks_fts(blocks_fts, rowid, text) VALUES ('delete', old.id, old.text);
  INSERT INTO blocks_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE VIRTUAL TABLE links_fts USING fts5(
  title, author, note_md, text_for_search, content='links', content_rowid='id'
);
CREATE TRIGGER links_ai AFTER INSERT ON links BEGIN
  INSERT INTO links_fts(rowid, title, author, note_md, text_for_search) VALUES (new.id, new.title, new.author, new.note_md, new.text_for_search);
END;
CREATE TRIGGER links_ad AFTER DELETE ON links BEGIN
  INSERT INTO links_fts(links_fts, rowid, title, author, note_md, text_for_search) VALUES ('delete', old.id, old.title, old.author, old.note_md, old.text_for_search);
END;
CREATE TRIGGER links_au AFTER UPDATE ON links BEGIN
  INSERT INTO links_fts(links_fts, rowid, title, author, note_md, text_for_search) VALUES ('delete', old.id, old.title, old.author, old.note_md, old.text_for_search);
  INSERT INTO links_fts(rowid, title, author, note_md, text_for_search) VALUES (new.id, new.title, new.author, new.note_md, new.text_for_search);
END;

CREATE VIRTUAL TABLE files_fts USING fts5(
  name, folder, content='files', content_rowid='id'
);
CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, name, folder) VALUES (new.id, new.name, new.folder);
END;
CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, folder) VALUES ('delete', old.id, old.name, old.folder);
END;
CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, folder) VALUES ('delete', old.id, old.name, old.folder);
  INSERT INTO files_fts(rowid, name, folder) VALUES (new.id, new.name, new.folder);
END;

CREATE VIRTUAL TABLE comments_fts USING fts5(
  body_md, content='comments', content_rowid='id'
);
CREATE TRIGGER comments_ai AFTER INSERT ON comments BEGIN
  INSERT INTO comments_fts(rowid, body_md) VALUES (new.id, new.body_md);
END;
CREATE TRIGGER comments_ad AFTER DELETE ON comments BEGIN
  INSERT INTO comments_fts(comments_fts, rowid, body_md) VALUES ('delete', old.id, old.body_md);
END;
CREATE TRIGGER comments_au AFTER UPDATE ON comments BEGIN
  INSERT INTO comments_fts(comments_fts, rowid, body_md) VALUES ('delete', old.id, old.body_md);
  INSERT INTO comments_fts(rowid, body_md) VALUES (new.id, new.body_md);
END;
