# Running

## Environment

Six variables are the whole bootstrap. Everything else the owner sets on
`/settings`, where it lives in the database. `.env.example` carries the same
list with the reasoning beside each one.

| Variable | What it is |
| --- | --- |
| `THESES_BIND` | Address the binary listens on. Inside the container leave it as `:8080`; the published port is what keeps it on loopback. |
| `THESES_DATA_DIR` | Where `theses.db` and the markdown mirror under `docs/` live. Thumbnails are written beside their originals in the bucket, not here. |
| `THESES_BASE_URL` | The absolute public origin, without a path, credentials, query, or fragment. Links in mail use it, and an `http` URL turns the `Secure` flag on cookies off so local development works. |
| `THESES_SECRET_KEY` | Encrypts settings secrets at rest with AES-GCM. At least 32 bytes. Losing it means losing every stored secret; it cannot be changed without re-entering them. |
| `THESES_SESSION_KEY` | Signs session cookies, CSRF tokens, and session and API token lookups. At least 32 bytes. Changing it signs everyone out. |
| `THESES_TRUST_PROXY` | Take the client address from the rightmost `X-Forwarded-For` entry rather than from the connection. |

Generate both keys with `openssl rand -hex 32`. Startup refuses a weak one.

`THESES_DEV` and `THESES_LOG_LEVEL` are optional and read by the binary.
`THESES_BIND_HOST`, `THESES_PORT` and `THESES_VERSION` are read by
`docker-compose.yml` and by nothing else. `/settings` lists the variables the
binary reads, read-only under Environment, with a line each on why it cannot
be edited there.

`TZ` sets the container's local time zone. `internal/config` never reads it;
the Go runtime does, which is what puts a local offset on the Date header of
outgoing mail. Release times and the digest run on the workspace time zone
set on `/settings`, not on this.

## Compose

```sh
docker compose up -d
docker compose logs -f
```

The container binds `:8080` inside and is published on `127.0.0.1:8080`, so
nothing off the host reaches it directly. One volume, `theses-data`, is
mounted at the data directory and holds the database and the markdown mirror.

## Behind a reverse proxy

Put any reverse proxy in front of the published port. One site block does it:
match the public host name, terminate TLS, forward to `127.0.0.1:8080`, pass
the client address through as `X-Forwarded-For`, and let websocket upgrades
through untouched. Nothing else about the request has to be rewritten.

Set `THESES_TRUST_PROXY=true` behind it. The client address that rate limiting
counts against and the activity log records is then the rightmost
`X-Forwarded-For` entry, which is the one the proxy appended; no other
forwarding header is believed. Set it to false if the port is ever published
anywhere but loopback, because then the header is whatever the caller typed
and any client could choose its own rate limit bucket.

## Local development

Pending database migrations run at startup. Migration `005_email_nocase.sql`
makes nonempty email addresses unique without regard to ASCII case and allows
multiple accounts with no email address. Existing case-variant duplicates make
that migration roll back and startup stop; resolve those account addresses in
the previous version before upgrading. Existing user IDs and their related
records are preserved.

```sh
cp .env.example .env
set -a; . ./.env; set +a
THESES_DEV=1 go run ./cmd/theses
```

Set `THESES_BASE_URL=http://localhost:8080` and `THESES_DATA_DIR=data` in
`.env` first. `THESES_DEV=1` reads templates and static files from `web/` and
reparses the templates on every render, so editing a page needs no restart. It
also stands the service worker down: assets are served under one unchanging
path with `no-store` on them, and a worker holding copies would serve this
morning's module through this afternoon's edit. Nothing is cached in
development, so nothing has to be cleared between edits.

Before every commit:

```sh
gofmt -l . && go vet ./... && go test ./...
```

## Documents on disk

Every document is mirrored to markdown under `docs/` in the data directory,
one directory per proposition and one file per document. The file opens with
the proposition, the document and the revision it was written at, and each
block carries a comment above it with its id and its version, so an edit made
at a terminal lands on exactly the block the browser would have written.

A file changed by anything else is read back in. The watcher waits two seconds
for the writing to settle, keeps a `pre-import` revision first, and applies
what changed as ordinary block commands recorded against no person, so every
open tab sees them arrive. Only paths this process has itself written are ever
imported, and a file whose contents are what this process last wrote is its own
echo and is left alone. A block the import could not take, because it had
changed on both sides, is written back with a conflict marker above it and the
version from the database in it.

Those comments are the boundary between one block and the next, so a line of
your own text that reads like one is written with a backslash in front of it
and read back without it, and a document about this file format is a document
like any other. Copying a paragraph in the file, comment line and all, makes a
new block of the copy and leaves the one it came from alone.

Two things follow if you edit the comments themselves. One you have indented is
no longer a comment, so the block it named loses its id and its words come back
as a block somebody added. And a bare comment line typed into the middle of a
block is a boundary: the block above keeps only what stood above that line,
which may be nothing at all, the words below it go to the block the comment
names, and that block's own words come back as a new block. Both are one
restore away, since an import keeps a `pre-import` revision before it writes.

## Background work

Six goroutines run beside the server and stop with it: the mail outbox, the
nightly backup, the markdown mirror and its watcher, the upload sweep, and the
notifier's two, one filling the notification outbox from every applied command
and one emptying it with bounded retries.

The sweep runs at startup and every hour after. It abandons an upload that has
been silent for 48 hours, which is 48 hours since anybody last asked for part
URLs rather than 48 hours of wall clock, and takes its parts out of the bucket
with it.

The notifier also runs one pass a day, a few minutes before the digest time set
on `/settings`: the cards due tomorrow, the cards that have just gone overdue,
and a release day tomorrow. The day it ran is recorded before the work, so a
restart an hour later does not send everything again.

## Deploying over a running version

Browsers hold the app shell, the stylesheet, the modules and the fonts in a
service worker cache. Nothing has to be cleared by hand. Every asset is served
under one prefix named after a hash of the whole static tree, the cache is
named after that hash, and the hash is written into `/sw.js` itself. A deploy
that changes any asset therefore changes the bytes of the worker, the browser
installs the new one on its next navigation, and activating it deletes every
cache that is not the current one. `/sw.js` is served with `no-cache` so the
browser always checks it.

The consequence worth knowing: a tab left open across a deploy keeps running
the old modules until it is reloaded, as it did before any of this. What it
cannot do is come back tomorrow and still be served them.

The one thing a deploy does not carry with it is the store browsers keep
offline work in, which is at version 2 from this version on: rolling back to a
build older than this one leaves whatever anybody had queued unreadable in
their browser, though untouched, until the newer build is served again.

## Health

`GET /healthz` answers 200 and the version as soon as the process is up. `GET
/readyz` runs the readiness checks and answers 503 with the failing check
named: the database, the object store, and the age of the newest backup, which
fails once it is over 36 hours old.

## Backups

A nightly job copies the database consistently, archives it with the markdown
mirror, encrypts the archive to a key derived from `THESES_SECRET_KEY`, and
uploads it with a manifest of counts, schema version and mirror revisions. The
destination is a prefix of the primary bucket, written with a second key that
can write and list but not delete, so neither a stolen application key nor a
bad settings change can take the history with it. Retention is the bucket's
own lifecycle and object lock rather than a delete from the app; thirty
archives are kept by default.

Restore runs from `/settings`, after a confirmation that names the archive and
its date. It stops writes, swaps the database, rewrites the mirror and starts
the watcher again. The archive is encrypted to the same key that encrypts the
stored secrets, so a restore needs `THESES_SECRET_KEY`, which the running app
needs anyway.

## Draft and notification recovery

Source-mode edits are kept as separate drafts on the current device. After a
reload, choose Recover draft to reopen the original text and revision base;
Save merges it with the current document. Drafts from different tabs are kept
separately. A draft is removed only after a confirmed save or explicit discard.
When browser storage is unavailable, keep the page open until Save succeeds.
Signing out clears the device's offline database, including drafts.

Notifications consume committed activity through a durable cursor. Matching,
queued messages, and cursor advancement share a transaction; activity
compaction waits until matching has passed a run. A restart or dropped bus
wakeup is recovered automatically. Recipients are checked against current
assignments and permissions. Delivery to external providers remains retryable,
so a lost provider response or historical restore may produce duplicates.

Owners can choose Verify restore beside a backup in Settings. It downloads,
decrypts, migrates, validates, and reconciles an isolated workspace without
replacing the running database or its documents. It checks up to ten referenced
file objects using the archived storage configuration and records the duration
and result. This samples current object availability, not every historical
object version. A timed-out or failed verification is recorded as a failure.
