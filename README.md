# THESES

A workspace per proposition: a board on top, shared markdown documents underneath, links and files beside them, several people editing live, and agents as first-class authors through a REST API and MCP.

## Status

Pre-alpha. Under active development; parts of this document and the docs it links describe what is being built.

## How it works

One Go binary and one process, with SQLite as the only database. Files go from the browser straight to S3-compatible object storage over presigned URLs, so no upload passes through the server. Documents are block lists in the database, merged three ways on the server and mirrored to markdown on disk. Six environment variables bootstrap the process and the owner configures the rest in the UI, where credentials are encrypted at rest. Every page is served under a strict CSP: no inline scripts, no third-party JavaScript, fonts self-hosted.

## Quickstart

```sh
cp .env.example .env
openssl rand -hex 32   # THESES_SECRET_KEY
openssl rand -hex 32   # THESES_SESSION_KEY
```

Put the two keys in `.env` and set `THESES_BASE_URL` to the absolute public URL, then:

```sh
docker compose up -d
```

Open it. The first visit is `/setup`: it creates the owner account, enrols an authenticator and signs you in. Every other route redirects there until an owner exists.

## Docs

- [Running](docs/running.md): environment variables, Compose, a reverse proxy in front, health endpoints, what to back up.
- [Settings](docs/settings.md): what each section of `/settings` configures, including the bucket CORS rule and SMTP.
- [Routes](docs/routes.md): every route the browser sees.
- [API](docs/api.md): `/api/v1` and `/mcp`, tokens and scopes.

MIT.
