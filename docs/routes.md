# Routes

What the browser sees. The machine surfaces, `/api/v1` and `/mcp`, are in
[api.md](api.md).

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
| `POST /settings/test/storage` | Write, read and delete a probe object in the chosen bucket. |
| `POST /settings/test/mail` | Send a test message to the signed-in owner through the configured SMTP. |
| `POST /settings/mail/retry` | Put every unsent message back at the front of the outbox. |
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
