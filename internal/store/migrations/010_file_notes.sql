ALTER TABLE files ADD COLUMN note_md TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN tags TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN metadata_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE files ADD COLUMN comment_revision INTEGER NOT NULL DEFAULT 0;

DROP TRIGGER files_ai;
DROP TRIGGER files_au;
DROP TRIGGER files_ad;
DROP TABLE files_fts;
CREATE VIRTUAL TABLE files_fts USING fts5(name, folder, note_md, tags, content='files', content_rowid='id');
CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
 INSERT INTO files_fts(rowid,name,folder,note_md,tags) VALUES(new.id,new.name,new.folder,new.note_md,new.tags);
END;
CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
 INSERT INTO files_fts(files_fts,rowid,name,folder,note_md,tags) VALUES('delete',old.id,old.name,old.folder,old.note_md,old.tags);
END;
CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN
 INSERT INTO files_fts(files_fts,rowid,name,folder,note_md,tags) VALUES('delete',old.id,old.name,old.folder,old.note_md,old.tags);
 INSERT INTO files_fts(rowid,name,folder,note_md,tags) VALUES(new.id,new.name,new.folder,new.note_md,new.tags);
END;
INSERT INTO files_fts(files_fts) VALUES('rebuild');

CREATE TABLE file_comments (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
 user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
 body_md TEXT NOT NULL,
 position_ms INTEGER NOT NULL CHECK(position_ms >= 0),
 created_at INTEGER NOT NULL
);
CREATE INDEX file_comments_file ON file_comments(file_id,position_ms,id);
