# Routes

What the browser sees. The machine surfaces, `/api/v1` and `/mcp`, are in
[api.md](api.md).

| Route | What it is |
| --- | --- |
| `GET /` | The app shell. |
| `GET /p/{id}` | Open a proposition: the shell with that one loaded. |
| `GET /p/{id}/settings` | The proposition's own settings: its members and its status. |
| `POST /p/{id}/settings` | Save them. |
| `GET /documents/{id}/revisions` | One document's history as JSON, for the History link above it. |
| `/app/...` | The links, files and attachment endpoints on the session cookie: the same handlers `/api/v1` serves. See [api.md](api.md). |
| `GET POST /setup` | First run only: create the owner. Every other route redirects here until one exists. |
| `GET POST /setup/authenticator` | Scan the QR code and confirm a code. The account is written only when the code matches. |
| `GET POST /login` | Account name, password and authenticator code, in one form. |
| `GET POST /login/authenticator` | Enroll before signing in, for an account this workspace requires an authenticator of and that has none. The session starts when a code from the new secret comes back. |
| `POST /logout` | End this browser's session. |
| `GET POST /reset` | Ask for a reset link by account name or email. Always answers the same. |
| `GET POST /reset/{token}` | Choose a new password. One use, one hour. |
| `GET POST /invite/{token}` | Accept an invitation: account name, name, initials, color, password. |
| `GET POST /invite/{token}/authenticator` | Enroll, then sign in. |
| `GET /profile` | You, security, danger. |
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
| `POST /settings/test/storage` | Write, read and delete a probe object in the chosen bucket. |
| `POST /settings/test/cors` | Hand this browser a presigned PUT and have it try the bucket, then remove the probe object. |
| `POST /settings/test/mail` | Send a test message to the signed-in owner through the configured SMTP. |
| `POST /settings/mail/retry` | Put every unsent message back at the front of the outbox. |
| `POST /settings/team/role` | Change someone's role. |
| `POST /settings/team/invite` | Send an invitation. |
| `POST /settings/team/invite/{id}/resend` | New token, new week, old link dead. |
| `POST /settings/team/invite/{id}/revoke` | Delete the invitation. |
| `POST /settings/tokens` | Create an API token. It is shown once. |
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
| `GET /ws?proposition={id}` | One websocket per tab, on the session cookie, subscribed to that proposition: presence, and every command as it is applied. A command frame is `{"id": 7, "cmd": "card.create", "key": "3f0a...", "args": {…}}`. `id` names the attempt and comes back on the answer; `key` names the change, is optional, and takes 1 to 64 letters, digits, hyphens or underscores. A frame sent again under a key the server has already answered is answered with what it did the first time, applying nothing, and that `ack`'s event carries `"replayed": true`. A key that is not one is an `error` frame. The server sends `{"type":"ping"}` every twenty five seconds and the tab answers `{"cmd":"pong"}`, which spends no part of its command allowance. A socket that has said nothing for sixty seconds is closed and the person behind it is no longer shown as present. A tab that is still there reconnects on its own and is present again. |
| `GET /offline` | What the service worker serves for a navigation the network refused that the shell cannot stand in for. |
| `GET /sw.js` | The service worker, from the root so its scope is the whole site. The URL never moves; the bytes carry the asset hash, so a deploy installs a new worker and the old cache goes with it. |
| `GET /shell` | The app with an empty payload, no account, no CSRF token and not even the workspace name. Anyone may fetch it. The worker keeps a copy and hands it to an offline navigation to `/` or `/p/{id}`; the page draws itself, the top bar included, from the snapshot in IndexedDB. |
| `GET /app/activity?proposition={id}` | The activity panel's read: the newest rows of one proposition, newest first, each saying whether it has been undone and whether an undo would be refused out of hand. Runs of saves made while somebody was typing are folded into one line by the panel, and once they are two days old they are folded into one row in the log itself. Session and membership, like the rest of `/app`. |
| `GET /app/search?q=&limit=` | The palette's read, as you type: the same grouped hits as `GET /api/v1/search`, over the propositions this person may read. `limit` is per kind. Session and membership, like the rest of `/app`. |
| `GET /healthz` | Always 200. |
| `GET /readyz` | Runs the readiness checks: the database, the object store, and the age of the newest backup. |
| `GET /static/{hash}/...` | Content-hashed assets, cached for a year. |

## Inside the shell

`GET /p/{id}` loads one proposition. Its tabs are views the shell renders in
the browser rather than routes of their own: the board, with the proposition's
documents beneath it, the links, and the files. They read and write over the
websocket and through `/app`, which is the API's own links and files handlers
on the session cookie, so moving between them costs no page load. A document's
history comes from `/documents/{id}/revisions`, and a tab that has lost its
websocket falls back to `/api/events`.

## Offline

The service worker caches the static tree, the offline page and the shell, and
nothing else: no API answer and no page rendered with a session on it. A
navigation is tried on the network first and falls back only when the network
refuses to answer at all, so a 404 or a 500 is still the server talking.

Commands made with no connection are applied in the browser, kept in an
IndexedDB outbox with the version and the text they started from, and replayed
in order over the websocket on reconnect. Each carries a name the tab picks, so
a command that was in the air when the socket went and goes up again is the
same command rather than a second one; the server remembers a name for 24 hours
and answers a repeat with what it did the first time. Two edits to one field
that fold into one outbox row take a new name, because what goes up is then a
different change. The server's three-way merge is what
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
