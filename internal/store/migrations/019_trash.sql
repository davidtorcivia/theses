CREATE TABLE trash (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
 entity TEXT NOT NULL,
 entity_id INTEGER NOT NULL,
 title TEXT NOT NULL,
 payload TEXT NOT NULL,
 deleted_at INTEGER NOT NULL,
 restored_at INTEGER,
 expires_at INTEGER NOT NULL
);
CREATE INDEX trash_proposition ON trash(proposition_id,id);
CREATE INDEX trash_expiry ON trash(expires_at);
