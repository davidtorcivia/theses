DROP TRIGGER cards_au;
CREATE TRIGGER cards_au AFTER UPDATE OF title,description_md ON cards
WHEN old.title IS NOT new.title OR old.description_md IS NOT new.description_md BEGIN
 INSERT INTO cards_fts(cards_fts,rowid,title,description_md) VALUES('delete',old.id,old.title,old.description_md);
 INSERT INTO cards_fts(rowid,title,description_md) VALUES(new.id,new.title,new.description_md);
END;
CREATE TABLE calendar_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 title TEXT NOT NULL,
 date TEXT NOT NULL,
 notes TEXT NOT NULL DEFAULT '',
 version INTEGER NOT NULL DEFAULT 1,
 created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
ALTER TABLE cards ADD COLUMN calendar_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cards ADD COLUMN calendar_updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE cards SET calendar_updated_at=created_at;
CREATE TRIGGER calendar_card_insert AFTER INSERT ON cards BEGIN
 UPDATE cards SET calendar_updated_at=unixepoch() WHERE id=new.id;
END;
CREATE TRIGGER calendar_card_update AFTER UPDATE OF title,due_date,done_at ON cards
WHEN old.title IS NOT new.title OR old.due_date IS NOT new.due_date OR old.done_at IS NOT new.done_at BEGIN
 UPDATE cards SET calendar_revision=calendar_revision+1,calendar_updated_at=unixepoch() WHERE id=new.id;
END;
