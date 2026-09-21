CREATE TABLE legal_releases (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proposition_id INTEGER NOT NULL REFERENCES propositions(id) ON DELETE RESTRICT,
 token TEXT NOT NULL UNIQUE,
 version INTEGER NOT NULL DEFAULT 1,
 state TEXT NOT NULL CHECK(state IN ('NY','GA')),
 title TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('street','interview','custom')),
 brand TEXT NOT NULL,
 rights_holder TEXT NOT NULL,
 details TEXT NOT NULL,
 body TEXT NOT NULL,
 closed INTEGER NOT NULL DEFAULT 0 CHECK(closed IN (0,1)),
 email_subject TEXT NOT NULL,
 email_body TEXT NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE INDEX legal_release_proposition ON legal_releases(proposition_id,id);
CREATE TABLE legal_submissions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 release_id INTEGER NOT NULL REFERENCES legal_releases(id) ON DELETE RESTRICT,
 receipt TEXT NOT NULL UNIQUE,
 request_hash TEXT NOT NULL,
 snapshot TEXT NOT NULL,
 people TEXT NOT NULL,
 signed_at INTEGER NOT NULL
);
CREATE INDEX legal_submission_release ON legal_submissions(release_id,id);
CREATE TABLE legal_notifications (
 release_id INTEGER NOT NULL REFERENCES legal_releases(id) ON DELETE RESTRICT,
 email TEXT NOT NULL,
 subject TEXT NOT NULL,
 body TEXT NOT NULL,
 queued_at INTEGER NOT NULL,
 PRIMARY KEY(release_id,email)
);
