# theses

A small production tool. One Go binary, SQLite, S3-compatible storage, Docker Compose behind a reverse proxy. MIT.

## Local development

```sh
cp .env.example .env
openssl rand -hex 32   # THESES_SECRET_KEY
openssl rand -hex 32   # THESES_SESSION_KEY
```

Put those two values in `.env`, set `THESES_BASE_URL=http://localhost:8080` and `THESES_DATA_DIR=data`, then:

```sh
set -a; . ./.env; set +a
THESES_DEV=1 go run ./cmd/theses
```

`THESES_DEV=1` reads templates and static files from `web/` and reparses the templates on every render, so editing a page needs no restart. An `http` base URL turns the `Secure` flag on cookies off, which is what makes a session work over plain localhost.

The first visit shows `/setup`: it creates the owner account, enrols an authenticator and signs you in. Until that is done every other route redirects there.

Mail is wired in a later step, so nothing is sent yet. Creating or resending an invitation shows its accept link once, on the Team section of `/settings`, for the owner to pass on; it is not shown again and not written to the log. A password reset writes its token and says nothing, so until mail lands a forgotten password is reset by an owner issuing a fresh invitation.

Before every commit:

```sh
gofmt -l . && go vet ./... && go test ./...
```

## On erebus

At `/nvme-mirror/apps/theses`, with `.env` filled in and `THESES_BASE_URL` set to the public hostname:

```sh
docker compose up -d
docker compose logs -f
```

The container binds `:8080` inside and is published on `127.0.0.1:8080`, so only the Caddy already on the host can reach it. One volume, `theses-data`, holds the database, the markdown mirror and the thumbnail cache. The Caddy site block is one line:

```
theses.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Set `THESES_TRUST_PROXY=true` behind that proxy so the client address used by rate limiting and the activity log comes from `X-Forwarded-For` or `CF-Connecting-IP` rather than the proxy's own.

## Configuration

Six environment variables are the whole bootstrap; everything else the owner sets on `/settings` and it lives in the database, with credentials encrypted by `THESES_SECRET_KEY`. `.env.example` documents each one. `/settings` lists them read-only under Environment with a line each on why they cannot be edited there.

## Routes

| Route | What it is |
| --- | --- |
| `GET /` | The app shell. The rail and the board arrive with the board step. |
| `GET POST /setup` | First run only: create the owner. Every other route redirects here until one exists. |
| `GET POST /setup/authenticator` | Scan the QR code and confirm a code. The account is written only when the code matches. |
| `GET POST /login` | Account name, password and authenticator code, in one form. |
| `POST /logout` | End this browser's session. |
| `GET POST /reset` | Ask for a reset link by account name or email. Always answers the same. |
| `GET POST /reset/{token}` | Choose a new password. One use, one hour. |
| `GET POST /invite/{token}` | Accept an invitation: account name, name, initials, colour, password. |
| `GET POST /invite/{token}/authenticator` | Enrol, then sign in. |
| `GET /profile` | You, security, danger. |
| `POST /profile` | Account name, name, initials, colour, email. |
| `POST /profile/password` | Change the password. |
| `POST /profile/totp` | Start enrolling a new authenticator. |
| `GET POST /profile/authenticator` | Scan and confirm it. |
| `POST /profile/signout-everywhere` | Bump the session epoch; every browser is signed out. |
| `POST /profile/delete` | Delete the account. The last owner cannot. |
| `GET /settings` | Owner only: Workspace, Defaults, Storage, Mail, Sign-in, Team, Environment. |
| `POST /settings` | Save every known key the form carried. |
| `POST /settings/test/storage` | Not wired yet; arrives with the files step. |
| `POST /settings/test/mail` | Not wired yet; arrives with the mail step. |
| `POST /settings/team/role` | Change someone's role. |
| `POST /settings/team/invite` | Send an invitation. |
| `POST /settings/team/invite/{id}/resend` | New token, new week, old link dead. |
| `POST /settings/team/invite/{id}/revoke` | Delete the invitation. |
| `POST /settings/tokens` | Create an API token. It is shown once. |
| `POST /settings/tokens/{id}/revoke` | Revoke one. |
| `GET /offline` | What the service worker will serve when the server is unreachable. |
| `GET /healthz` | Always 200. |
| `GET /readyz` | Runs the readiness checks. The database now; the object store and the backup age later. |
| `GET /static/{hash}/...` | Content-hashed assets, cached for a year. |

`/api/v1` and `/mcp` are the next thing added.

MIT.