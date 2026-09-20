-- The text a block held at a version is recovered from the activity log, which
-- every stale block.set asks for. Saving as somebody types makes that read
-- ordinary rather than rare, and without this it is a scan of the whole table.
CREATE INDEX activity_entity ON activity(entity, entity_id, id);
