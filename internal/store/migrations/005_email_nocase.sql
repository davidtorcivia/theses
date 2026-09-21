CREATE TABLE users_email_nocase_rebuild (
  id             INTEGER PRIMARY KEY,
  handle         TEXT    NOT NULL,
  email          TEXT    NOT NULL,
  name           TEXT    NOT NULL,
  initials       TEXT    NOT NULL,
  colour         TEXT    NOT NULL,
  role           TEXT    NOT NULL CHECK (role IN ('owner','editor','researcher','guest')),
  password_hash  TEXT    NOT NULL,
  totp_secret    TEXT    NOT NULL DEFAULT '',
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  session_epoch  INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  last_seen_at   INTEGER
);

INSERT INTO users_email_nocase_rebuild
  (id, handle, email, name, initials, colour, role, password_hash, totp_secret,
   totp_last_step, session_epoch, created_at, last_seen_at)
SELECT id, handle, email, name, initials, colour, role, password_hash, totp_secret,
       totp_last_step, session_epoch, created_at, last_seen_at
FROM users;

DROP TABLE users;
ALTER TABLE users_email_nocase_rebuild RENAME TO users;

CREATE UNIQUE INDEX users_handle_unique ON users(handle);
CREATE UNIQUE INDEX users_email_nocase ON users(lower(email)) WHERE email <> '';
