-- Ordering keys are fractional indexes (internal/frac), which are short base-62
-- strings. A column declared REAL has REAL affinity, so SQLite would silently
-- turn the key "1" into 1.0 and leave "1A" as text, and the two would no longer
-- compare against each other. The four ordered board tables therefore take TEXT.
--
-- They are recreated rather than copied because nothing creates a proposition,
-- a column, a card or a checklist item before this migration: the board lands
-- with it. The activity table does hold rows from setup and settings, so that
-- one is copied across; it is rebuilt only to admit the 'file' actor the
-- document watcher will use.
--
-- With foreign keys on, DROP TABLE prepares the delete its children cascade
-- from, and preparing anything resolves every foreign key in the schema. So a
-- dropped table is recreated before the next statement that reads the schema,
-- and the triggers that write to cards_fts come out first because they name a
-- table whose content is about to go missing.

CREATE TABLE activity_new (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER REFERENCES propositions(id) ON DELETE CASCADE,
  actor_kind     TEXT    NOT NULL CHECK (actor_kind IN ('user','token','mcp','file','system')),
  actor_id       TEXT    NOT NULL DEFAULT '',
  entity         TEXT    NOT NULL,
  entity_id      TEXT    NOT NULL DEFAULT '',
  action         TEXT    NOT NULL,
  before_json    TEXT,
  after_json     TEXT,
  created_at     INTEGER NOT NULL,
  undone_at      INTEGER
);
INSERT INTO activity_new SELECT * FROM activity;
DROP TABLE activity;
ALTER TABLE activity_new RENAME TO activity;
CREATE INDEX activity_created ON activity(created_at);
CREATE INDEX activity_proposition ON activity(proposition_id, id);

DROP TRIGGER cards_ai;
DROP TRIGGER cards_ad;
DROP TRIGGER cards_au;

DROP TABLE propositions;
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
  position    TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,
  archived_at INTEGER
);

DROP TABLE columns;
CREATE TABLE columns (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  position       TEXT    NOT NULL
);
CREATE INDEX columns_proposition ON columns(proposition_id, position);

DROP TABLE cards;
CREATE TABLE cards (
  id             INTEGER PRIMARY KEY,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  column_id      INTEGER NOT NULL REFERENCES columns(id) ON DELETE CASCADE,
  position       TEXT    NOT NULL,
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
CREATE INDEX cards_proposition ON cards(proposition_id);

DROP TABLE checklist_items;
CREATE TABLE checklist_items (
  id       INTEGER PRIMARY KEY,
  card_id  INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  text     TEXT    NOT NULL,
  done     INTEGER NOT NULL DEFAULT 0,
  position TEXT    NOT NULL
);
CREATE INDEX checklist_items_card ON checklist_items(card_id, position);

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
