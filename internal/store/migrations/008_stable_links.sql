-- Preserve link identities after deleting the highest numbered item.

DROP TRIGGER "block_texts_ai";

DROP TRIGGER "block_texts_au";

DROP TRIGGER "blocks_ad";

DROP TRIGGER "blocks_ai";

DROP TRIGGER "blocks_au";

DROP TRIGGER "blocks_revision_ai";

DROP TRIGGER "blocks_revision_au";

DROP TRIGGER "cards_ad";

DROP TRIGGER "cards_ai";

DROP TRIGGER "cards_au";

DROP TRIGGER "comments_ad";

DROP TRIGGER "comments_ai";

DROP TRIGGER "comments_au";

DROP TRIGGER "files_ad";

DROP TRIGGER "files_ai";

DROP TRIGGER "files_au";

DROP TRIGGER "links_ad";

DROP TRIGGER "links_ai";

DROP TRIGGER "links_au";

CREATE TABLE cards_stable (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
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

INSERT INTO cards_stable ("id","proposition_id","column_id","position","title","description_md","question","due_date","done_at","created_by","created_at","version") SELECT "id","proposition_id","column_id","position","title","description_md","question","due_date","done_at","created_by","created_at","version" FROM cards;

DROP TABLE cards;

ALTER TABLE cards_stable RENAME TO cards;

CREATE INDEX cards_column ON cards(column_id, position);

CREATE INDEX cards_proposition ON cards(proposition_id);

CREATE TABLE comments_stable (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  card_id    INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
  body_md    TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);

INSERT INTO comments_stable ("id","card_id","user_id","body_md","created_at") SELECT "id","card_id","user_id","body_md","created_at" FROM comments;

DROP TABLE comments;

ALTER TABLE comments_stable RENAME TO comments;

CREATE INDEX comments_card ON comments(card_id, created_at);

CREATE TABLE documents_stable (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  slug           TEXT    NOT NULL,
  position       REAL    NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at     INTEGER NOT NULL,
  revision       INTEGER NOT NULL DEFAULT 0,
  UNIQUE (proposition_id, slug)
);

INSERT INTO documents_stable ("id","proposition_id","name","slug","position","created_by","created_at","revision") SELECT "id","proposition_id","name","slug","position","created_by","created_at","revision" FROM documents;

DROP TABLE documents;

ALTER TABLE documents_stable RENAME TO documents;

CREATE TABLE blocks_stable (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  position    TEXT    NOT NULL,
  text        TEXT    NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  updated_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER
);

INSERT INTO blocks_stable ("id","document_id","position","text","version","updated_by","updated_at","deleted_at") SELECT "id","document_id","position","text","version","updated_by","updated_at","deleted_at" FROM blocks;

DROP TABLE blocks;

ALTER TABLE blocks_stable RENAME TO blocks;

CREATE INDEX blocks_document ON blocks(document_id, position);

CREATE TABLE links_stable (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
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

INSERT INTO links_stable ("id","proposition_id","url","canonical_url","title","author","year","kind","note_md","question","added_by","created_at","fetched_at","text_for_search") SELECT "id","proposition_id","url","canonical_url","title","author","year","kind","note_md","question","added_by","created_at","fetched_at","text_for_search" FROM links;

DROP TABLE links;

ALTER TABLE links_stable RENAME TO links;

CREATE INDEX links_proposition ON links(proposition_id);

CREATE TABLE files_stable (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
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

INSERT INTO files_stable ("id","proposition_id","name","folder","kind","size","object_key","version_of","duration_ms","width","height","uploaded_by","state","created_at") SELECT "id","proposition_id","name","folder","kind","size","object_key","version_of","duration_ms","width","height","uploaded_by","state","created_at" FROM files;

DROP TABLE files;

ALTER TABLE files_stable RENAME TO files;

CREATE INDEX files_proposition ON files(proposition_id);

CREATE TRIGGER block_texts_ai AFTER INSERT ON blocks BEGIN
  INSERT OR REPLACE INTO block_texts (block_id, version, text) VALUES (new.id, new.version, new.text);
  DELETE FROM block_texts WHERE block_id = new.id AND version <= new.version - 20;
END;

CREATE TRIGGER block_texts_au AFTER UPDATE OF text, version ON blocks BEGIN
  INSERT OR REPLACE INTO block_texts (block_id, version, text) VALUES (new.id, new.version, new.text);
  DELETE FROM block_texts WHERE block_id = new.id AND version <= new.version - 20;
END;

CREATE TRIGGER blocks_ad AFTER DELETE ON blocks BEGIN
  INSERT INTO blocks_fts(blocks_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;

CREATE TRIGGER blocks_ai AFTER INSERT ON blocks BEGIN
  INSERT INTO blocks_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER blocks_au AFTER UPDATE ON blocks BEGIN
  INSERT INTO blocks_fts(blocks_fts, rowid, text) VALUES ('delete', old.id, old.text);
  INSERT INTO blocks_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER blocks_revision_ai AFTER INSERT ON blocks BEGIN
  UPDATE documents SET revision = revision + 1 WHERE id = new.document_id;
END;

CREATE TRIGGER blocks_revision_au AFTER UPDATE ON blocks BEGIN
  UPDATE documents SET revision = revision + 1 WHERE id = new.document_id;
END;

CREATE TRIGGER cards_ad AFTER DELETE ON cards BEGIN
  INSERT INTO cards_fts(cards_fts, rowid, title, description_md) VALUES ('delete', old.id, old.title, old.description_md);
END;

CREATE TRIGGER cards_ai AFTER INSERT ON cards BEGIN
  INSERT INTO cards_fts(rowid, title, description_md) VALUES (new.id, new.title, new.description_md);
END;

CREATE TRIGGER cards_au AFTER UPDATE ON cards BEGIN
  INSERT INTO cards_fts(cards_fts, rowid, title, description_md) VALUES ('delete', old.id, old.title, old.description_md);
  INSERT INTO cards_fts(rowid, title, description_md) VALUES (new.id, new.title, new.description_md);
END;

CREATE TRIGGER comments_ad AFTER DELETE ON comments BEGIN
  INSERT INTO comments_fts(comments_fts, rowid, body_md) VALUES ('delete', old.id, old.body_md);
END;

CREATE TRIGGER comments_ai AFTER INSERT ON comments BEGIN
  INSERT INTO comments_fts(rowid, body_md) VALUES (new.id, new.body_md);
END;

CREATE TRIGGER comments_au AFTER UPDATE ON comments BEGIN
  INSERT INTO comments_fts(comments_fts, rowid, body_md) VALUES ('delete', old.id, old.body_md);
  INSERT INTO comments_fts(rowid, body_md) VALUES (new.id, new.body_md);
END;

CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, folder) VALUES ('delete', old.id, old.name, old.folder);
END;

CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, name, folder) VALUES (new.id, new.name, new.folder);
END;

CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, folder) VALUES ('delete', old.id, old.name, old.folder);
  INSERT INTO files_fts(rowid, name, folder) VALUES (new.id, new.name, new.folder);
END;

CREATE TRIGGER links_ad AFTER DELETE ON links BEGIN
  INSERT INTO links_fts(links_fts, rowid, title, author, note_md, text_for_search) VALUES ('delete', old.id, old.title, old.author, old.note_md, old.text_for_search);
END;

CREATE TRIGGER links_ai AFTER INSERT ON links BEGIN
  INSERT INTO links_fts(rowid, title, author, note_md, text_for_search) VALUES (new.id, new.title, new.author, new.note_md, new.text_for_search);
END;

CREATE TRIGGER links_au AFTER UPDATE ON links BEGIN
  INSERT INTO links_fts(links_fts, rowid, title, author, note_md, text_for_search) VALUES ('delete', old.id, old.title, old.author, old.note_md, old.text_for_search);
  INSERT INTO links_fts(rowid, title, author, note_md, text_for_search) VALUES (new.id, new.title, new.author, new.note_md, new.text_for_search);
END;


-- Deleted rows may still have links in retained activity.

INSERT INTO sqlite_sequence(name,seq) SELECT 'cards',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='cards');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='card'),0)) WHERE name='cards';

INSERT INTO sqlite_sequence(name,seq) SELECT 'comments',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='comments');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='comment'),0)) WHERE name='comments';

INSERT INTO sqlite_sequence(name,seq) SELECT 'documents',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='documents');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='document'),0)) WHERE name='documents';

INSERT INTO sqlite_sequence(name,seq) SELECT 'blocks',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='blocks');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='block'),0)) WHERE name='blocks';

INSERT INTO sqlite_sequence(name,seq) SELECT 'links',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='links');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='link'),0)) WHERE name='links';

INSERT INTO sqlite_sequence(name,seq) SELECT 'files',0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='files');
UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity='file'),0)) WHERE name='files';
