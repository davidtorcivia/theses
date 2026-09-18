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
| `GET /ws?proposition={id}` | One websocket per tab, on the session cookie, subscribed to that proposition: presence, and every command as it is applied. |
| `GET /offline` | What the service worker will serve when the server is unreachable. |
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
