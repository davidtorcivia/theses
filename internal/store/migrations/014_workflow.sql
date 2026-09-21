CREATE TABLE script_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
 document_id INTEGER NOT NULL,
 fingerprint TEXT NOT NULL,
 markdown TEXT NOT NULL,
 cues TEXT NOT NULL DEFAULT '',
 created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
 created_at INTEGER NOT NULL
);
CREATE INDEX script_snapshots_document ON script_snapshots(document_id,id);
CREATE TABLE reviews (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
 document_id INTEGER,
 file_id INTEGER,
 snapshot_id INTEGER REFERENCES script_snapshots(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL,
 reviewer_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
 state TEXT NOT NULL CHECK(state IN ('needs_review','changes_requested','approved')),
 note TEXT NOT NULL DEFAULT '',
 version INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL,
 decided_at INTEGER,
 CHECK((document_id IS NULL) != (file_id IS NULL))
);
CREATE INDEX reviews_proposition ON reviews(proposition_id,id);
ALTER TABLE file_comments ADD COLUMN resolved_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE file_comments ADD COLUMN resolved_at INTEGER;
ALTER TABLE file_comments ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
CREATE TABLE evidence (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE CASCADE,
 link_id INTEGER REFERENCES links(id) ON DELETE SET NULL,
 file_id INTEGER REFERENCES files(id) ON DELETE SET NULL,
 block_id INTEGER REFERENCES blocks(id) ON DELETE SET NULL,
 title TEXT NOT NULL,
 author TEXT NOT NULL DEFAULT '',
 year TEXT NOT NULL DEFAULT '',
 url TEXT NOT NULL DEFAULT '',
 quotation TEXT NOT NULL DEFAULT '',
 locator TEXT NOT NULL DEFAULT '',
 interpretation TEXT NOT NULL DEFAULT '',
 claim TEXT NOT NULL DEFAULT '',
 verified INTEGER NOT NULL DEFAULT 0,
 verified_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
 version INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX evidence_proposition ON evidence(proposition_id,id);
CREATE INDEX files_ready_version ON files(version_of) WHERE state='ready';
