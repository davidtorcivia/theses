CREATE TABLE production_templates (
 proposition_id INTEGER PRIMARY KEY REFERENCES propositions(id) ON DELETE CASCADE,
 card_id INTEGER UNIQUE REFERENCES cards(id) ON DELETE SET NULL
);
