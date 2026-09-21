# THESES

A workspace per proposition: a board on top, shared markdown documents underneath, links and files beside them, several people editing live, and agents as first-class authors through a REST API and MCP.

The shared **Show** workspace at `/show` holds the overall kanban, notes and file repository. It is pinned above the proposition list and available to every account, with editing controlled by each account's role. Type `@` in notes or card text to pick a person or proposition. **+ Proposition** adds a linked card whose title and status update with the proposition; moving that card tracks show work without changing the proposition's production status.

## Status

Under active development. The guides below describe implemented behavior; dated audit reports record historical findings and validation. See [workflow and recovery boundaries](docs/agent-recovery.md#boundaries) for current limitations.

## How it works

The app runs as one Go binary with SQLite as its only database. Browser uploads go straight to S3-compatible object storage over presigned URLs. Drive imports copy through the app, and optional local transcription sends recordings to a separate operator-controlled Whisper service. Documents are block lists in the database, merged three ways on the server and mirrored to markdown on disk. Six core environment variables bootstrap the process; optional runtime settings are documented in [Running](docs/running.md). The owner configures workspace settings in the UI, where credentials are encrypted at rest. Every page is served under a strict CSP: no inline scripts, no third-party JavaScript, fonts self-hosted.

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

Open it. The first visit is `/setup`: it creates the owner account, enrolls an authenticator and signs you in. Workspace pages redirect there until an owner exists; health endpoints and public agent guides remain available.

## Docs

- [Running](docs/running.md): environment variables, Compose, a reverse proxy in front, health endpoints, what to back up.
- [Settings](docs/settings.md): what each section of `/settings` configures, including the bucket CORS rule and SMTP.
- [Routes](docs/routes.md): every route the browser sees.
- [Workflows](docs/workflows.md): Show notes, My work, production plans, research, recording and reviews.
- [Recording releases](docs/legal.md): public signing links, branded QR cards, saved agreements and manual participant emails.
- [Calendar](docs/calendar.md): events, tasks, U.S. holidays and Google/iCloud subscriptions.
- [Transcription](docs/transcription.md): optional local Whisper, speaker-label limits and Pinecast handoff.
- [Connections](docs/connections.md): personal keys, expiry and Claude Desktop setup.
- [Agent guide](docs/SKILLS.md): practical REST/MCP workflows, permissions and retries. Also served publicly at `/SKILLS.md`.
- [API](docs/api.md): `/api/v1` and `/mcp`, request contracts, tokens and scopes. Also served at `/api.md`; connection instructions are at `/connections.md`.
- [Recovery](docs/agent-recovery.md): recently deleted content, safe retries, validation and limitations.
- [Quality](docs/quality.md): local checks, CI, browser tests and measured performance.
- Historical audits: [initial](docs/audit.md) and [follow-up](docs/audit-followup.md).

MIT.
