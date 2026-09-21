# Routes

What the browser sees. The machine surfaces, `/api/v1` and `/mcp`, are in
[api.md](api.md).

| Route | What it is |
| --- | --- |
| `GET /` | The app shell, opening Show. |
| `GET /show` | The shared Show board, notes and files. |
| `GET POST /show/settings` | Show name, description and board columns. |
| `GET /p/{id}` | Open a proposition: the shell with that one loaded. |
| `GET /p/{id}/settings` | The proposition's own settings: its members and its status. |
| `POST /p/{id}/settings` | Save them. |
| `GET /documents/{id}/revisions` | One document's history as JSON, for the History link above it. |
| `/app/...` | Session-cookie endpoints for source editing, links, files, attachments, production plans, calendar entries/tasks, evidence, snapshots, reviews, transcripts, transcription jobs and trash. They share REST handlers; mutations require CSRF protection. See [api.md](api.md) for the corresponding `/api/v1` contracts. |
| `POST /app/commands` | HTTP fallback for command frames when WebSockets are unavailable. Uses the session cookie, CSRF protection, and the same command, idempotency key, rate limit, and `ack`/`conflict`/`error` response envelopes as `/ws`. The browser reads missed events through `/api/events` while using this transport. Presence requires WebSockets. |
| `GET POST /setup` | First run only: create the owner. Workspace pages redirect here until one exists; health endpoints and public guides remain available. |
| `GET POST /setup/authenticator` | Scan the QR code and confirm a code. The account is written only when the code matches. |
| `GET POST /login` | Account name, password and authenticator code, in one form. |
| `GET POST /login/authenticator` | Enroll before signing in, for an account this workspace requires an authenticator of and that has none. The session starts when a code from the new secret comes back. |
| `POST /logout` | End this browser's session. |
| `GET POST /reset` | Ask for a reset link by account name or email. Always answers the same. |
| `GET POST /reset/{token}` | Choose a new password. One use, one hour. |
| `GET POST /invite/{token}` | Accept an invitation: account name, name, initials, color, password. |
| `GET POST /invite/{token}/authenticator` | Enroll, then sign in. |
| `GET /profile` | Account, security, notifications, calendar sync and personal API/MCP keys with client setup instructions. |
| `POST /profile/tokens` | Create a personal API/MCP key with permitted scopes and optional expiry; shown once. |
| `POST /profile/tokens/{id}/revoke` | Revoke one of your personal keys. |
| `POST /profile/calendar` | Create, replace or revoke your private calendar subscription. |
| `GET /calendar/{token}/production.ics` | Credential-bearing, read-only calendar feed; does not require a session. See [calendar.md](calendar.md). |
| `POST /profile` | Account name, name, initials, color, email. |
| `POST /profile/password` | Change the password. |
| `POST /profile/totp` | Start enrolling a new authenticator. |
| `GET POST /profile/authenticator` | Scan and confirm it. |
| `POST /profile/signout-everywhere` | Bump the session epoch; every browser is signed out. |
| `POST /profile/delete` | Delete the account. The last owner cannot. |
| `POST /profile/notifications` | Save the matrix: which of your channels hears about what. |
| `POST /profile/notifications/channel` | Add one of your channels or change it. A secret left blank keeps the stored one. |
| `POST /profile/notifications/channel/{id}/test` | Send one message to it and mark it verified when it arrives. |
| `POST /profile/notifications/channel/{id}/delete` | Remove it. |
| `GET /settings` | Owner only. See [settings.md](settings.md). |
| `POST /settings` | Save every known key the form carried. |
| `POST /settings/cors` | Apply and read back the CORS rule for the saved bucket configuration. |
| `POST /settings/test/storage` | Write, read and delete a probe object in the chosen bucket. |
| `POST /settings/test/cors` | Hand this browser a presigned PUT and have it try the bucket, then remove the probe object. |
| `POST /settings/test/mail` | Send a test message to the signed-in owner through the configured SMTP. |
| `POST /settings/mail/retry` | Put every unsent message back at the front of the outbox. |
| `POST /settings/test/backups` | Test the separate backup storage key. |
| `POST /settings/backups/now` | Start an immediate backup. |
| `POST /settings/backups/verify` | Verify an archive in isolation without replacing live data. |
| `POST /settings/backups/restore` | Restore an archive; invalidates API keys and calendar subscriptions. |
| `POST /settings/team/role` | Change someone's role. |
| `POST /settings/team/invite` | Send an invitation. |
| `POST /settings/team/invite/{id}/resend` | New token, new week, old link dead. |
| `POST /settings/team/invite/{id}/revoke` | Delete the invitation. |
| `POST /settings/tokens` | Create an API token with permitted scopes and optional expiry. It is shown once. |
| `POST /settings/tokens/{id}/revoke` | Revoke one. |
| `POST /settings/notifications/defaults` | What a new account's email starts subscribed to. |
| `POST /settings/integrations/webhook` | Add a workspace webhook or change it: its URL, its secret, the events it fires on and the column it watches. |
| `POST /settings/integrations/webhook/{id}/test` | Send one message to it and mark it verified when it arrives. |
| `POST /settings/integrations/webhook/{id}/delete` | Remove it. |
| `POST /settings/integrations/drive/connect` | Start the Drive authorization: a random value in a cookie, the same value as the state parameter, and a redirect to Google. |
| `GET /settings/integrations/drive/callback` | Where Google sends the owner back. The code is exchanged only when the state matches this browser's cookie, which is spent either way. |
| `POST /settings/integrations/drive/disconnect` | Throw the Drive token away and keep the client id. |
| `POST /settings/integrations/transistor/disconnect` | Throw the Transistor key away. |
| `POST /settings/test/drive` | List the root folder of the connected Drive. |
| `POST /settings/test/transistor` | Fetch the show, or, with no show id saved, list the ones the key reaches. |
| `POST /p/{id}/publish` | Save which document and which recording this proposition publishes with, and, with `do=publish`, send it to Transistor. |
| `GET /app/drive` | One Drive folder's contents, or a search. For anybody who may edit. |
| `POST /app/drive/import` | Copy one Drive file into the bucket as a file on a proposition. |
| `GET /ws?proposition={id}` | One websocket per tab, on the session cookie, subscribed to that proposition: presence, and every command as it is applied. A command frame is `{"id": 7, "cmd": "card.create", "key": "3f0a...", "args": {…}}`. `id` names the attempt and comes back on the answer; `key` names the change, is optional, and takes 1 to 64 letters, digits, hyphens or underscores. A frame sent again under a key the server has already answered is answered with what it did the first time, applying nothing, and that `ack`'s event carries `"replayed": true`. A key that is not one is an `error` frame. Every event a keyed command produces carries that key back as `"key"`: on the `ack` to the tab that sent it, on the copy every other tab in the room is sent, and on the same row read later out of the stream, so a tab that drew something before the server had it knows the row wherever it meets it. `block.insert` takes `after_key` in place of `after`, which is the key the command that makes the block above it went up under: a tab with no connection draws the block it has just made and queues the insert, so a second block made under the first has only that key to name it by. The server sends `{"type":"ping"}` every twenty five seconds and the tab answers `{"cmd":"pong"}`, which spends no part of its command allowance. A socket that has said nothing for sixty seconds is closed and the person behind it is no longer shown as present. A tab that is still there reconnects on its own and is present again. |
| `GET /api/events?proposition={id}&since={seq}` | Session-authenticated event catch-up used by the browser fallback transport. |
| `GET /app/production-plans` | Production plans for accessible active propositions. |
| `GET /app/production?proposition={id}` | Production checklist card, ready-recording count and plan. |
| `GET /app/my-work` | Your unfinished assigned cards across accessible active propositions. |
| `GET /app/backlinks?proposition={id}` | Accessible cards and note passages referring to the proposition. |
| `GET /SKILLS.md`, `GET /api.md`, `GET /connections.md` | Public agent, API and connection guides, including before setup; contain no workspace data or credentials. |
| `GET /offline` | What the service worker serves for a navigation the network refused that the shell cannot stand in for. |
| `GET /sw.js` | The service worker, from the root so its scope is the whole site. The URL never moves; the bytes carry the asset hash, so a deploy installs a new worker and the old cache goes with it. |
| `GET /shell` | The app with an empty payload, no account, no CSRF token and not even the workspace name. Anyone may fetch it. The worker keeps a copy and hands it to an offline navigation to `/`, `/show` or `/p/{id}`; the page draws itself, the top bar included, from the snapshot in IndexedDB. |
| `GET /app/activity?proposition={id}` | The activity panel's read: the newest rows of one proposition, newest first, each saying whether it has been undone and whether an undo would be refused out of hand. Runs of saves made while somebody was typing are folded into one line by the panel, and once they are two days old they are folded into one row in the log itself. Session and membership, like the rest of `/app`. |
| `GET /app/search?q=&limit=` | The palette's read, as you type: the same grouped hits as `GET /api/v1/search`, over the propositions this person may read. `limit` is per kind. Session and membership, like the rest of `/app`. |
| `GET /healthz` | Always 200. |
| `GET /readyz` | Runs the readiness checks: the database, the object store, and the age of the newest backup. |
| `GET /static/{hash}/...` | Content-hashed assets, cached for a year. |

## Inside the shell

`GET /p/{id}` loads one proposition. Its tabs are views the shell renders in
the browser rather than routes of their own: the board, with the proposition's
documents beneath it, the links, and the files. They read and write over the
websocket and through `/app`, which exposes shared API handlers on the session
cookie, so moving between them costs no page load. Show uses the same inline
notes layout. Production calendar, My work, research and recording controls are
also browser views rather than separate navigation routes. Activity toggles
open and closed and includes Recently deleted. See [workflows.md](workflows.md). A document's
history comes from `/documents/{id}/revisions`, and a tab that has lost its
websocket falls back to `/api/events`.

## Offline

The service worker caches the static tree, the offline page and the shell, and
nothing else: no API answer and no page rendered with a session on it. A
navigation is tried on the network first and falls back only when the network
refuses to answer at all, so a 404 or a 500 is still the server talking.

Supported board/document commands and link creation made with no connection
are applied in the browser and kept in an IndexedDB outbox with the version and the text they started from, and replayed
in order over the websocket on reconnect. Each carries a name the tab picks, so
a command that was in the air when the socket went and goes up again is the
same command rather than a second one; the server remembers a name for 24 hours
and answers a repeat with what it did the first time. Two edits to one field
that fold into one outbox row take a new name, because what goes up is then a
different change.

A block made with no connection, by Enter in the middle of a paragraph, by the
+ after the last block or by pasting several paragraphs, is drawn at once and
its `block.insert` waits in the outbox like any other command. Until the server
has made it the block is on this device only: nobody else sees it, what is
typed into it is written into the command that is waiting, and a block made
under it names it by that command's name rather than by an id nobody has yet.
It is kept in the cached proposition, so a reload with no connection still
draws it. It goes the moment the real block arrives carrying that name, which
every tab of that person sees and acts on, so the paragraph is never on a page
twice. The server's three-way merge is what
settles a set that went stale meanwhile; a replay it refuses appears in the
activity panel with keep mine and take theirs, the same choice a live conflict
offers. A replay the server refuses for a reason of its own, being busy or
being broken, is retried rather than recorded as a decision, and the queue goes
up at a pace the socket's own limit allows.

What this device holds goes when the session does, because the next person at
the machine has no session and should find nothing of the workspace. Two things
see to it: the worker deletes the database on the sign-out request itself, and
the page deletes it and returns to the sign-in page the moment the server
answers a request with no session behind it, which is what an expired or
revoked one looks like. Every queued command carries the account that made it,
so work left by one person is never sent as another. The cache survives both,
since every byte in it is the app itself and names nobody.

Not every mutation queues offline. File/link metadata edits, attachments and
workflow requests need a connection. Source drafts persist separately, while
uncertain workflow retry keys generally live only in the open UI. See
[recovery boundaries](agent-recovery.md#boundaries) before closing or reloading
an operation whose result is unknown.
