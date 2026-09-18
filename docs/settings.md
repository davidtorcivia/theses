# Settings

`/settings` is owner only. Every value on it is written to the settings table
and read back typed; a key the registry does not know cannot be written at
all. Secrets are encrypted with AES-GCM under a key derived from
`THESES_SECRET_KEY` and never go back out: the page shows a set secret as set,
and the API and MCP listings report it the same way rather than returning it.

## Workspace

The name the workspace is known by, the number episode numbering starts at,
the release day and release time, and the IANA time zone those and the digest
schedule are read in.

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

Whether everyone or only owners has to enrol an authenticator, how many days a
sign-in lasts before it has to be repeated, and the shortest account name
allowed. Owners are always required to enrol.

## Team

Owners change roles and settings, editors do everything else, researchers
cannot delete, and guests read. An invitation is queued as mail and its accept
link is also shown once on the page, for the owner to pass on by hand; neither
link is written to the activity log. Resending mints a new token and kills the
old link.

API tokens are created here too. A token is shown once, carries the
permissions of the account that made it, and can be revoked. See
[api.md](api.md).

## Environment

The bootstrap variables, read once at startup and listed read-only with a line
each on why they cannot be edited here. See [running.md](running.md).
