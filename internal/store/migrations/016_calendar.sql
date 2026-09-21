CREATE TABLE calendar_subscriptions (
 user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL UNIQUE
);
ALTER TABLE propositions ADD COLUMN calendar_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE propositions ADD COLUMN calendar_updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE propositions SET calendar_updated_at=created_at;
CREATE TRIGGER calendar_proposition_insert AFTER INSERT ON propositions BEGIN
 UPDATE propositions SET calendar_updated_at=unixepoch() WHERE id=new.id;
END;
CREATE TRIGGER calendar_proposition_update AFTER UPDATE OF title,target_date ON propositions
WHEN old.title IS NOT new.title OR old.target_date IS NOT new.target_date BEGIN
 UPDATE propositions SET calendar_revision=calendar_revision+1,calendar_updated_at=unixepoch() WHERE id=new.id;
END;
CREATE TRIGGER calendar_plan_insert AFTER INSERT ON production_plans BEGIN
 UPDATE propositions SET calendar_revision=calendar_revision+1,calendar_updated_at=unixepoch() WHERE id=new.proposition_id;
END;
CREATE TRIGGER calendar_plan_update AFTER UPDATE OF record_date,edit_date ON production_plans
WHEN old.record_date IS NOT new.record_date OR old.edit_date IS NOT new.edit_date BEGIN
 UPDATE propositions SET calendar_revision=calendar_revision+1,calendar_updated_at=unixepoch() WHERE id=new.proposition_id;
END;
CREATE TRIGGER calendar_plan_delete AFTER DELETE ON production_plans BEGIN
 UPDATE propositions SET calendar_revision=calendar_revision+1,calendar_updated_at=unixepoch() WHERE id=old.proposition_id;
END;
