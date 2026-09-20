-- The text a block held at a version, which is the base of the three way merge
-- a stale block.set is decided by. It used to be recovered from the activity
-- log, where saving as somebody types writes the whole paragraph about once a
-- second; the log is now folded, and a fold takes the intermediate texts with
-- it, so the bases live here instead.
--
-- It is filled by triggers rather than by the command, so every path that
-- writes a block's text is covered: the blocks a new document starts from,
-- SetBlock, an undo putting the columns back, an import from the markdown
-- mirror, and whatever is written next year.
CREATE TABLE block_texts (
  block_id INTEGER NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
  version  INTEGER NOT NULL,
  text     TEXT    NOT NULL,
  PRIMARY KEY (block_id, version)
) WITHOUT ROWID;

-- Twenty versions back. An editor's base is at most a few saves behind, and an
-- offline edit older than twenty saves of the same paragraph is a conflict,
-- which is the honest answer by then.
CREATE TRIGGER block_texts_ai AFTER INSERT ON blocks BEGIN
  INSERT OR REPLACE INTO block_texts (block_id, version, text) VALUES (new.id, new.version, new.text);
  DELETE FROM block_texts WHERE block_id = new.id AND version <= new.version - 20;
END;
CREATE TRIGGER block_texts_au AFTER UPDATE OF text, version ON blocks BEGIN
  INSERT OR REPLACE INTO block_texts (block_id, version, text) VALUES (new.id, new.version, new.text);
  DELETE FROM block_texts WHERE block_id = new.id AND version <= new.version - 20;
END;
