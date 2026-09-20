-- A command can reach the server twice. A tab whose socket dies with a frame in
-- flight puts that command back in the outbox and sends it again, an agent
-- retries a request that timed out, and the server may already have applied
-- what is being sent again. For a field being set to a value that is the second
-- write of the same value; for a create it is a second card, a second link, a
-- second paragraph.
--
-- So the client picks a key and the server remembers which activity row that key
-- produced. A command arriving under a key it has already seen is answered with
-- the event it was answered with the first time and applies nothing.
--
-- The key belongs to the actor, so two people, or two tokens of two people,
-- cannot collide on one. The activity row is the reference rather than the
-- entity, because there is one activity table and a dozen entity tables, and
-- because the cascade then says what should happen when the log is compacted:
-- a key whose row is gone becomes a miss, which replays the command, which is
-- the safe answer a day later.
CREATE TABLE client_keys (
  actor_id    INTEGER NOT NULL,
  key         TEXT    NOT NULL,
  activity_id INTEGER NOT NULL REFERENCES activity(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (actor_id, key)
) WITHOUT ROWID;

-- Keys are pruned by age, which is the only query that reads them by anything
-- but the primary key.
CREATE INDEX client_keys_created ON client_keys(created_at);

-- The cascade looks its rows up by activity_id, and a foreign key with no index
-- on the child column costs a scan of the whole child table for every parent
-- row that goes.
CREATE INDEX client_keys_activity ON client_keys(activity_id);
