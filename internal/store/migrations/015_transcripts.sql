CREATE TABLE transcripts (
 file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
 segments TEXT NOT NULL,
 text TEXT NOT NULL,
 version INTEGER NOT NULL DEFAULT 1
);
CREATE VIRTUAL TABLE transcripts_fts USING fts5(text,content='transcripts',content_rowid='file_id');
CREATE TRIGGER transcripts_ai AFTER INSERT ON transcripts BEGIN INSERT INTO transcripts_fts(rowid,text) VALUES(new.file_id,new.text); END;
CREATE TRIGGER transcripts_ad AFTER DELETE ON transcripts BEGIN INSERT INTO transcripts_fts(transcripts_fts,rowid,text) VALUES('delete',old.file_id,old.text); END;
CREATE TRIGGER transcripts_au AFTER UPDATE ON transcripts BEGIN INSERT INTO transcripts_fts(transcripts_fts,rowid,text) VALUES('delete',old.file_id,old.text); INSERT INTO transcripts_fts(rowid,text) VALUES(new.file_id,new.text); END;
CREATE TABLE transcription_jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
 user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','running','complete','failed')),
 error TEXT NOT NULL DEFAULT '',
 stereo INTEGER NOT NULL DEFAULT 0,
 base_version INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX transcription_active ON transcription_jobs(file_id) WHERE state IN ('queued','running');
CREATE VIRTUAL TABLE evidence_fts USING fts5(title,quotation,interpretation,claim,content='evidence',content_rowid='id');
INSERT INTO evidence_fts(rowid,title,quotation,interpretation,claim) SELECT id,title,quotation,interpretation,claim FROM evidence;
CREATE TRIGGER evidence_ai AFTER INSERT ON evidence BEGIN INSERT INTO evidence_fts(rowid,title,quotation,interpretation,claim) VALUES(new.id,new.title,new.quotation,new.interpretation,new.claim); END;
CREATE TRIGGER evidence_ad AFTER DELETE ON evidence BEGIN INSERT INTO evidence_fts(evidence_fts,rowid,title,quotation,interpretation,claim) VALUES('delete',old.id,old.title,old.quotation,old.interpretation,old.claim); END;
CREATE TRIGGER evidence_au AFTER UPDATE ON evidence BEGIN INSERT INTO evidence_fts(evidence_fts,rowid,title,quotation,interpretation,claim) VALUES('delete',old.id,old.title,old.quotation,old.interpretation,old.claim); INSERT INTO evidence_fts(rowid,title,quotation,interpretation,claim) VALUES(new.id,new.title,new.quotation,new.interpretation,new.claim); END;
