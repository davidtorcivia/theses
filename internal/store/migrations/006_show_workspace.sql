ALTER TABLE propositions ADD COLUMN kind TEXT NOT NULL DEFAULT 'proposition'
  CHECK (kind IN ('proposition', 'show'));

CREATE TABLE proposition_sequence (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  last_id   INTEGER NOT NULL
);
INSERT INTO proposition_sequence (singleton, last_id)
VALUES (1, max(
  coalesce((SELECT max(id) FROM propositions), 0),
  coalesce((SELECT max(CAST(entity_id AS INTEGER)) FROM activity WHERE entity = 'proposition'), 0)
));

CREATE UNIQUE INDEX propositions_one_show ON propositions(kind) WHERE kind = 'show';

CREATE TRIGGER show_members_existing
AFTER INSERT ON propositions
WHEN NEW.kind = 'show'
BEGIN
  INSERT OR IGNORE INTO proposition_members (proposition_id, user_id)
    SELECT NEW.id, id FROM users;
END;

CREATE TRIGGER show_members_new_user
AFTER INSERT ON users
BEGIN
  INSERT OR IGNORE INTO proposition_members (proposition_id, user_id)
    SELECT id, NEW.id FROM propositions WHERE kind = 'show';
END;

CREATE TRIGGER show_members_keep
AFTER DELETE ON proposition_members
WHEN EXISTS (SELECT 1 FROM propositions WHERE id = OLD.proposition_id AND kind = 'show')
 AND EXISTS (SELECT 1 FROM users WHERE id = OLD.user_id)
BEGIN
  INSERT OR IGNORE INTO proposition_members (proposition_id, user_id)
    VALUES (OLD.proposition_id, OLD.user_id);
END;

CREATE TRIGGER show_keep
BEFORE DELETE ON propositions
WHEN OLD.kind = 'show'
BEGIN
  SELECT RAISE(ABORT, 'the Show workspace is permanent');
END;

CREATE TRIGGER show_keep_shape
BEFORE UPDATE OF kind, number, status, episode, target_date, position, archived_at ON propositions
WHEN OLD.kind = 'show' AND (
  NEW.kind IS NOT OLD.kind OR NEW.number IS NOT OLD.number OR NEW.status IS NOT OLD.status OR
  NEW.episode IS NOT OLD.episode OR NEW.target_date IS NOT OLD.target_date OR
  NEW.position IS NOT OLD.position OR NEW.archived_at IS NOT OLD.archived_at
)
BEGIN
  SELECT RAISE(ABORT, 'the Show workspace is permanent');
END;
