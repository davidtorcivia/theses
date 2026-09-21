# Running

## Environment

Six core variables bootstrap the app. Workspace configuration lives in the
database and is edited on `/settings`. `.env.example` documents the core
variables; optional runtime and transcription settings are described below.

| Variable | What it is |
| --- | --- |
| `THESES_BIND` | Address the binary listens on. Inside the container leave it as `:8080`; the published port is what keeps it on loopback. |
| `THESES_DATA_DIR` | Where `theses.db` and the markdown mirror under `docs/` live. Thumbnails are written beside their originals in the bucket, not here. |
| `THESES_BASE_URL` | The absolute public origin, without a path, credentials, query, or fragment. Links in mail use it, and an `http` URL turns the `Secure` flag on cookies off so local development works. |
| `THESES_SECRET_KEY` | Encrypts settings secrets at rest with AES-GCM. At least 32 bytes. Losing it means losing every stored secret; it cannot be changed without re-entering them. |
| `THESES_SESSION_KEY` | Signs session cookies, CSRF tokens, and session and API token lookups. At least 32 bytes. Changing it signs everyone out. |
| `THESES_TRUST_PROXY` | Take the client address from the rightmost `X-Forwarded-For` entry rather than from the connection. |

Generate both keys with `openssl rand -hex 32`. Startup refuses a weak one.

`THESES_DEV`, `THESES_LOG_LEVEL`, and `THESES_WHISPER_URL` are optional and read by the binary. The last enables a trusted local inference endpoint; see [transcription.md](transcription.md) for the optional Compose service, model directory, device group and resource limits.
`THESES_BIND_HOST`, `THESES_PORT` and `THESES_VERSION` are read by
`docker-compose.yml` and by nothing else. `/settings` lists the core bootstrap
variables read-only under Environment. Optional transcription is configured by
the operator, outside that form.

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
test -z "$(gofmt -l .)" && go vet ./... && go test ./... && node --test web/*_test.mjs
```

See [quality.md](quality.md) for JavaScript syntax checks, browser tests, races, dependency checks and the production image gate.

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

Background workers handle the mail outbox, nightly backup, markdown mirror
and watcher, housekeeping, notification matching and delivery, and optional
local transcription. Notification matching catches up from committed activity
through a durable cursor; delivery uses bounded retries. Workers drain or stop
with the server. Transcription runs one job at a time when configured.

The sweep runs at startup and every hour after. It abandons an upload that has
been silent for 48 hours, which is 48 hours since anybody last asked for part
URLs rather than 48 hours of wall clock, and takes its parts out of the bucket
with it.

The notifier also runs one pass a day, a few minutes before the digest time set
on `/settings`: the cards due tomorrow, the cards that have just gone overdue,
and a release day tomorrow. Reminder intents and their daily marker commit together, so a
restart an hour later does not queue the same daily pass again.

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
offline work in. Its current schema is version 4, including source drafts and
saved preferences. Rolling back to a build that opens a lower IndexedDB
version leaves queued work unreadable, though untouched, until a compatible
build is served again. Recover or export unsaved work before such a rollback.

## Health

`GET /healthz` answers 200 and the version as soon as the process is up. `GET
/readyz` runs the readiness checks and answers 503 with the failing check
named: the database, the object store, and the age of the newest backup, which
fails once it is over 36 hours old when nightly backups are enabled. Enabled backups also fail readiness until one has succeeded; disabled backups skip the age check.

## Backups

A nightly job copies the database consistently, archives it with the markdown
mirror, encrypts the archive to a key derived from `THESES_SECRET_KEY`, and
uploads it with a manifest of counts, schema version and mirror revisions. The
destination is a prefix of the primary bucket, written with a second key that
can write and list but not delete, so neither a stolen application key nor a
bad settings change can take the history with it. Retention is the bucket's
own lifecycle and Object Lock rather than a delete from the app. The retention
setting defaults to 30 days and expresses the intended bucket policy; configure
that policy at the storage provider. The app does not enforce a 30-archive cap.
Archives contain database and markdown data, not the file objects themselves;
protect the primary and recordings buckets separately.

Restore runs from `/settings`, after a confirmation that names the archive and
its date. It stops writes, swaps the database, rewrites the mirror and starts
the watcher again. The archive is encrypted to the same key that encrypts the
stored secrets, so a restore needs `THESES_SECRET_KEY`, which the running app
needs anyway. Restoring clears API keys and calendar subscription credentials
so previously revoked secrets cannot become valid again. Recreate keys and
subscriptions after a restore. Interrupted transcription jobs require explicit
retry. The app retains replaced files under timestamped aside paths; inspect
any partial-restore error before retrying or removing those files.

## Deleted content recovery

**Activity → Recently deleted** restores individual cards, documents, links,
completed files, evidence and calendar events for seven days after deletion.
This applies to deletions made with recovery enabled, not older deletion
history. Restoring requires delete permission on the original workspace. A
conflicting restore is refused atomically. File bytes must still exist in the
bucket. Proposition deletion, individual comments/checklists and incomplete
uploads are outside this feature. See [recovery boundaries](agent-recovery.md#boundaries).

The owner storage-cleanup preview lists recorded, app-owned unreferenced keys
with a seven-day grace period. Cleanup rechecks references, protects active
trash, and shares a maintenance reservation with restore. It does not discover
every orphan in a bucket or replace backups.

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
