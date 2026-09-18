# Running

## Environment

Six variables are the whole bootstrap. Everything else the owner sets on
`/settings`, where it lives in the database. `.env.example` carries the same
list with the reasoning beside each one.

| Variable | What it is |
| --- | --- |
| `THESES_BIND` | Address the binary listens on. Inside the container leave it as `:8080`; the published port is what keeps it on loopback. |
| `THESES_DATA_DIR` | Where `theses.db` and the markdown mirror under `docs/` live. Thumbnails are written beside their originals in the bucket, not here. |
| `THESES_BASE_URL` | The absolute public URL, no trailing slash. Links in mail use it, and an `http` URL turns the `Secure` flag on cookies off so local development works. |
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

```sh
cp .env.example .env
set -a; . ./.env; set +a
THESES_DEV=1 go run ./cmd/theses
```

Set `THESES_BASE_URL=http://localhost:8080` and `THESES_DATA_DIR=data` in
`.env` first. `THESES_DEV=1` reads templates and static files from `web/` and
reparses the templates on every render, so editing a page needs no restart.

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
