CREATE TABLE activity_new (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  proposition_id INTEGER REFERENCES propositions(id) ON DELETE CASCADE,
  actor_kind     TEXT    NOT NULL CHECK (actor_kind IN ('user','file','system')),
  actor_id       TEXT    NOT NULL DEFAULT '',
  via            TEXT,
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
CREATE INDEX activity_entity ON activity(entity, entity_id, id);

CREATE TABLE notification_cursor (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  activity_id INTEGER NOT NULL
);
INSERT INTO notification_cursor (id, activity_id)
VALUES (1, (SELECT coalesce(max(id), 0) FROM activity));
