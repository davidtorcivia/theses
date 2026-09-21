CREATE TABLE production_plans (
 proposition_id INTEGER PRIMARY KEY REFERENCES propositions(id) ON DELETE CASCADE,
 owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
 next_action TEXT NOT NULL DEFAULT '',
 blocker TEXT NOT NULL DEFAULT '',
 record_date TEXT NOT NULL DEFAULT '',
 edit_date TEXT NOT NULL DEFAULT '',
 version INTEGER NOT NULL DEFAULT 1
);
