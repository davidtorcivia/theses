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
key. The endpoint takes either a full `https://` URL or a bare host. The
public base URL is optional, for a CDN in front of the bucket.

The browser uploads to the bucket directly over a presigned URL, so the bucket
needs a CORS rule allowing `PUT`, `GET` and `HEAD` from the origin in
`THESES_BASE_URL`. The section prints the exact rule for the chosen provider.
**Apply the CORS rule**, with the test buttons under the fields, sets it on
that bucket with the saved keys through `POST /settings/cors` and reads it
back, so the notice says what the bucket holds rather than what was sent to it.
It applies what is saved, not what is typed, so save the fields first. It
replaces any rule already on the bucket, and there is one button per bucket
that has a name, so a separate recordings bucket gets its own. A key that may
not write bucket settings is refused by the provider, and the section prints
that sentence and "The rule can still be applied by hand." A put the provider
takes but will not read back is not a failure: the notice says the rule went
and to look again in a minute. On Backblaze, a rule set through the native API
is neither returned by `GetBucketCors` nor replaced by `PutBucketCors`, so a
read back may find nothing where the native API shows a rule.
`POST /settings/test/storage` writes, reads and deletes a probe object and
reports what happened.

**Check CORS from this browser** is the half the server cannot do for itself:
the page asks for a presigned PUT, has this browser try it against the bucket
the way an upload does, and then asks again to have the probe object removed.
A bucket whose CORS rule is missing or names another origin fails here rather
than on somebody's first upload.

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
name allowed.

The requirement is enforced at the sign-in itself. Setup and invitation
acceptance both enroll before the account row is written, so an invited account
always has an authenticator whatever this is set to; what the setting decides
is what happens to an account that has none, which is one whose role changed
or whose secret was removed. With **Everyone**, any such account is sent to
enroll as soon as its password is accepted and signs in at the end of it. With
**Owners only**, an editor, researcher or guest signs straight in, and an owner
is sent to enroll: owners are always required, because this setting is theirs to
change.

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

One row per service, with configure, connect, a test button and disconnect.
Every key, token and secret here is stored the way the rest of this page stores
one: encrypted at rest, shown as set, never sent back to the browser and never
written to a log or into an error message.

**Google Drive**, files in. Make an OAuth client of type Web application in a
Google Cloud project with the Drive API enabled, list the redirect address the
section prints as an authorized redirect URI, and paste the client id and
secret. Connect then sends you to Google to approve read-only access to the
Drive account the files are in, and the refresh token that comes back is what
every later request is made on; it is renewed on its own and stored again the
same way. The test button lists the root folder. Once it is connected, anybody
who can edit a proposition gets **add from Drive** in its files pane: a dialog
listing one folder at a time, or a search, and an import that copies the chosen
file into the bucket as an ordinary file on that proposition, attributed to
whoever pressed it. Native Google Docs, Sheets and Slides files are not listed,
because they have no file to copy until they are exported. Disconnect throws
the token away and keeps the client id, so connecting again is one button.

**Pinecast** is a manual publishing handoff: when no legacy Transistor key is configured, proposition settings link to the Pinecast dashboard. Prepare audio, show notes and transcript exports here, then upload and schedule there. There is no Pinecast API connection, credential field or automatic publishing in this build. See [workflows.md](workflows.md).

**Transistor**, optional legacy publishing. Paste an API key and the show's id; testing with
the show field empty prints the shows the key reaches, with their ids, so the
id can be read off the page. The last field is the status at which a
proposition may be published, which is `released` unless it is changed. A
connected installation shows a Publish section on proposition settings; its
publish action becomes available at the configured status. Choose a document for the show notes, defaulting to the one called Show notes,
and a recording from the Recordings folder for the audio. Publishing creates
the episode with the proposition's title and blurb, the notes rendered to HTML,
and a presigned link to the recording that lasts a day, which is how Transistor
fetches a file out of a private bucket. The episode id is kept on the
proposition, so publishing again updates that episode instead of making
another, and the page links to it.

**Riverside** and **Descript** integrations are not implemented.

**Webhooks** are below the integration rows: the workspace fires those whoever
caused the thing, which is how a chat room or anything else is wired up without
an integration of its own. Each has a URL, a secret each message is signed with
when one is set, the events it fires on, and optionally one column: a card move
then fires only when the card lands there. A webhook is sent nothing until a
test message has reached it, and the page tests one as it is saved, so a URL
that refuses says so on the spot.

## Backups

The destination, nightly schedule, intended retention in days, a manual-run
button, and archive verification and restore controls. Configure lifecycle and
Object Lock retention at the bucket provider; the app does not delete archives
to enforce the setting. Verify restore checks an isolated copy without replacing
the workspace. Restoring revokes API keys and calendar subscription credentials. See [running.md](running.md) for what an archive contains and
what a restore does.

## Team

Owners change roles and settings, editors do everything else, researchers
cannot delete, and guests read. An invitation is queued as mail and its accept
link is also shown once on the page, for the owner to pass on by hand; neither
link is written to the activity log. Resending mints a new token and kills the
old link.

Each user creates and revokes personal API/MCP keys in **Profile → API & MCP**. The owner workspace page lists all unrevoked keys, including expired ones, with the owning account and can revoke any of them. Keys are shown once and carry the intersection of their selected scopes and the account's current permissions. Choose 7, 30, 90 or 365 days, or no expiry; new forms default to 30 days. Existing keys retain their lifetime. Expiry and permission changes apply on the next REST or MCP request, including an initialized MCP session. Restoring a backup clears API keys and calendar subscription credentials to avoid reactivating revoked secrets. See [connection setup](connections.md) and [api.md](api.md).

## Operations and personal settings

Owner settings include aggregate diagnostics and the storage-cleanup preview.
Process-lifetime failure counters are historical counts, not current-health
alarms; see [quality.md](quality.md). Cleanup only considers recorded app-owned
keys, waits seven days, and rechecks references and active trash before removal.

Each account configures notification channels, quiet hours, personal API/MCP
keys and its read-only calendar subscription in Profile. Calendar subscriptions
are separate credentials from API keys; see [calendar.md](calendar.md).

## Environment

The bootstrap variables, read once at startup and listed read-only with a line
each on why they cannot be edited here. See [running.md](running.md).

## Recording releases

Workspace settings include the default rights-holding legal entity and participant email subject/body templates. New releases copy those defaults; existing links retain their saved values. Placeholders are `{name}`, `{brand}`, `{title}` and `{url}`. See [Recording releases](legal.md) for the signing and manual email workflow.
