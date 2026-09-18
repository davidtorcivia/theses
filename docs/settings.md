# Settings

`/settings` is owner only. Every value on it is written to the settings table
and read back typed; a key the registry does not know cannot be written at
all. Secrets are encrypted with AES-GCM under a key derived from
`THESES_SECRET_KEY` and never go back out: the page shows a set secret as set,
and the API and MCP listings report it the same way rather than returning it.

## Workspace

The name the workspace is known by, the number episode numbering starts at,
the release day and release time, and the IANA time zone the release time and
the digest schedule are read in.

## Defaults

What a new proposition starts with: the board columns, the statuses in the
order a proposition moves through them, the four question labels a card can be
tagged with, and the document template a new proposition's document is created
from. Existing boards and documents keep their own.

## Storage

Where files and recordings live. There is a primary bucket and, optionally, a
second one for recordings; leaving the recordings bucket empty keeps
recordings in the primary. Each takes a provider (`backblaze`, `r2` or a plain
`s3` endpoint), an endpoint, a region, a bucket, an access key and a secret
key. The public base URL is optional, for a CDN in front of the bucket.

The browser uploads to the bucket directly over a presigned URL, so the bucket
needs a CORS rule allowing `PUT`, `GET` and `HEAD` from the origin in
`THESES_BASE_URL`. The section prints the exact rule for the chosen provider;
apply it on the bucket before the first upload. `POST /settings/test/storage`
writes, reads and deletes a probe object and reports what happened.

The second button is the half the server cannot do for itself: the page asks
for a presigned PUT, has this browser try it against the bucket the way an
upload does, and then asks again to have the probe object removed. A bucket
whose CORS rule is missing or names another origin fails here rather than on
somebody's first upload.

Changing a bucket does not move what is already in the old one.

## Mail

Invitations, password resets and notifications go out through one SMTP server:
host, port, TLS (`starttls`, `tls` or `none`), username, password and the From
address. Sending is queued through an outbox, so a slow or unreachable server
never fails a request. Until the host and the From address are filled in,
messages sit in the outbox and the page says so. `POST /settings/test/mail`
sends a test message to the signed-in owner, and `POST /settings/mail/retry`
puts every unsent message back at the front of the outbox.

## Sign-in

Whether an authenticator is required of everyone or of owners only, how many
days a sign-in lasts before it has to be repeated, and the shortest account
name allowed. Owners always enrol, whatever the first is set to.

## Notifications

What a new account's email starts subscribed to, ticked from the same list of
events the profile page draws its matrix from, so that someone who changes
nothing still hears about what is addressed to them. Then the pieces every
account's own channels borrow: the Pushover application token, so that members
paste only their own user key; the ntfy server a topic lives on when nobody
names one; and the time of day the daily digest and the due date pass run, read
in the workspace time zone. Each account picks its own channels, its quiet
hours and its rules on its profile page.

## Integrations

Webhooks the workspace fires whoever caused the thing, which is how a chat
room or anything else is wired up without an integration of its own. Each has
a URL, a secret each message is signed with when one is set, the events it
fires on, and optionally one column: a card move then fires only when the card
lands there. A webhook is sent nothing until a test message has reached it, and
the page tests one as it is saved, so a URL that refuses says so on the spot.
The secret is stored on the same terms as the rest of this page.

## Backups

The destination, the time of day the nightly run starts, how many archives to
keep, a button that runs one now, and the list of archives with a restore
beside each. See [running.md](running.md) for what an archive contains and
what a restore does.

## Team

Owners change roles and settings, editors do everything else, researchers
cannot delete, and guests read. An invitation is queued as mail and its accept
link is also shown once on the page, for the owner to pass on by hand; neither
link is written to the activity log. Resending mints a new token and kills the
old link.

API tokens and MCP clients are managed here too. A token is shown once,
carries the permissions of the account that made it, and can be revoked. See
[api.md](api.md).

## Environment

The bootstrap variables, read once at startup and listed read-only with a line
each on why they cannot be edited here. See [running.md](running.md).
