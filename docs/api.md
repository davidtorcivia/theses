# THESES API

A REST API under `/api/v1` and an MCP server at `/mcp`. Both take the same
bearer tokens and the same scopes, and both write to the same activity log as
the browser does.

## Tokens and scopes

A token is created in the app and shown once. It starts with `thes_` and is sent
on every request:

```
Authorization: Bearer thes_...
```

Scopes are `read`, `write`, `files` and `admin`. `admin` implies the others. A
route says which scope it needs; a token without it is refused. A token also
cannot do what the person it belongs to may not do, so demoting someone refuses
their tokens too: after a demotion to guest, their admin token is refused the
settings routes and keeps the ones that only read.

One refusal has one status across the whole API, whichever resource it came
from.

| Status | When |
| --- | --- |
| 400 | the body is not JSON, does not have the field the route reads, or carries an `Idempotency-Key` that is not a key |
| 401 | no `Authorization: Bearer` header, or the token is unknown or revoked |
| 403 | the token does not have the scope the route needs, the person it belongs to no longer has the standing that scope implies, or the row is somebody else's note |
| 404 | no such endpoint, no such settings key, or a thing that is not there or that the token's owner may not touch |
| 409 | the thing changed while you were editing it, or its state refuses the change: an archived proposition, a column with cards still in it, a change that cannot be undone |
| 413 | the request body is over 64 KiB, which `/api/v1` and `/mcp` both allow |
| 422 | the body is JSON and the rules refuse it: a title that is empty or too long, a kind or a question that is not on the list, a size no upload may be |
| 429 | over 300 requests a minute for one token |
| 500 | a fault on the server; the detail is in its log, not in the response |
| 503 | object storage has not been set up yet, so the route that needs it cannot answer |

Not there and not allowed are both `404`. The commands answer the role and the
membership with one refusal, so telling the two apart would tell a caller
whether a row it may not read exists.

Every refusal is JSON with one field:

```json
{"error": "this token does not have the admin scope"}
```

## Sending the same change twice

A request whose answer never came back leaves a caller with no way of knowing
whether the change happened. Naming the change is how it finds out without
guessing:

```
Idempotency-Key: 3f0a1b2c-4d5e-6f70-8192-a3b4c5d6e7f8
```

1 to 64 letters, digits, hyphens or underscores; anything else is `400`. Send
the same request again under the same key and the server answers with what the
first one did, having applied nothing. The event in that answer carries
`"replayed": true`:

```json
{"event": {"seq": 412, "entity": "card", "entity_id": 88, "action": "create",
           "replayed": true, "...": "..."}}
```

A key names one request, not one endpoint. The same key sent with a different
body is still a repeat: the answer is what the first request did, and the
second change is not made. Use a fresh key for every change you mean to make,
and the same one only for sending the same change again.

The event a keyed write answers with carries the key back as `"key"`, and so do
the copies of that event on the websocket, on the event stream and in the
activity the browser reads. A caller that drew something before it could know
what the server would call it recognizes the row wherever it first meets it,
rather than only in the answer to its own request. A request that made several
rows, which is text with a blank line in it, spent the key and then the key with
`#2` and `#3` on the end, one per row, and each event says which of them it was.
Everybody who may read the event sees that field. A key is spent against the
person who chose it, so nobody else can send a command under it or collide with
it; all it says is that the row was made by a client that named the change.

The key belongs to the person the token belongs to, not to the token. Two
tokens of one person share one set of keys, so an agent holding two of them
should not reuse a key between them; two different people cannot collide.

A key is remembered for 24 hours. Past that a repeat is applied again: this
makes a retry safe, it is not a record kept for ever. A read ignores the
header.

Every write that goes through the command log honors it: the board, the
documents, the links, the attachments, undo and creating a file. `PUT
/api/v1/settings/{key}` and the notification routes are not commands and ignore
it, and sending the same setting or the same rule twice sets it to what it
already holds.

The answer to a repeat is the answer the route gives, not a copy of the first
response body:

- a route that answers with an `event` answers with the first one's event, with
  `"replayed": true` on it;
- the attachment routes answer `{"card": …, "action": "attach"}` as they always
  do, read off that event;
- `PUT /api/v1/documents/{id}/source` answers `{"base": [], "conflicts": [],
  "merged": [], "replayed": true}`. It is many commands in one transaction and nothing
  remembers what the first answer said, so a caller that means to write more
  reads the document again first;
- `POST /api/v1/files` makes no second file row and no second upload. For a
  file small enough for one `PUT` it signs the same object key again; for a
  multipart upload it answers the resume, which is the same `upload_id`, the
  parts already in the bucket and fresh URLs for the ones that are not. The
  answer is therefore the one `GET /api/v1/files/{id}/parts` gives, and either
  will do to carry on with.

A repeat of a write whose own effect moved the thing out of reach is refused
before the key is looked at, and the refusal is the answer. Sending
`DELETE /api/v1/cards/{id}` again is `404`, because the card is gone; sending
`POST /api/v1/files/{id}/complete` again is `422`, `that upload is not in a
state for this`, because the file is ready. In both the change did happen,
which is what the caller wanted to know. The key is what stops a repeat making
a second thing; it does not turn every refusal into a replay.

## `GET /api/v1/me`

Any token. Reports the token and the person it belongs to.

```json
{
  "token": {"id": 3, "name": "research agent", "scopes": ["read", "write"]},
  "user": {"id": 1, "handle": "nora", "name": "Nora Vance",
           "email": "nora@example.com", "role": "owner"}
}
```

## `GET /api/v1/users`

Scope `read`. Everyone in the workspace. Email addresses are not included; a
token sees its own owner's address in `/me` and no one else's.

```json
{"users": [{"id": 1, "handle": "nora", "name": "Nora Vance", "initials": "NV",
            "colour": "#b45", "role": "owner", "last_seen_at": 1758067200}]}
```

## `GET /api/v1/search?q=&limit=`

Scope `read`. Full text search over cards, document blocks, links, files and
comments, plus propositions and people matched by name. `limit` is per kind,
10 by default and 50 at most. Results are limited to the propositions the
token's owner is a member of; an owner searches every one. People are found by
anyone who may search at all, because a person belongs to no proposition.

`q` is cut into words at every character that is not a letter or a digit, and
each word is matched as written. Punctuation is therefore dropped from the full
text query rather than parsed as syntax: quotation marks, asterisks, colons,
carets and parentheses do not reach FTS5, and `NEAR`, `AND`, `OR` and `NOT` are
searched for as ordinary words. The last word is matched as a prefix, so `deb`
finds `debt`.

Propositions and people are not searched that way: they are matched on `q` as
typed, punctuation and all, anywhere inside the title or the name. A `q` with no
letter or digit in it returns no groups at all.

```json
{
  "query": "debt",
  "groups": [
    {"kind": "proposition", "hits": [
      {"kind": "proposition", "id": 10, "title": "Student debt is a policy choice",
       "snippet": "It was designed", "proposition_id": 10}]},
    {"kind": "card", "hits": [
      {"kind": "card", "id": 4, "title": "Find the debt numbers",
       "snippet": "Federal loan totals…", "proposition_id": 10}]}
  ]
}
```

Kinds, in the order they are returned: `proposition`, `card`, `block`, `link`,
`file`, `comment`, `user`. A kind with no hits is left out. A block's `title` is
the document it is in; a comment's is the card it is on. Snippets are plain
text with an ellipsis where they were cut.

## `GET /api/v1/activity?since=&limit=`

Scope `read`. The activity log as a cursor, not a feed: the rows after `since`
in id order. `since` is the id of the last row you have seen, `0` or absent for
the beginning. `limit` is 50 by default and 200 at most. Poll by asking again
with the last id you were given. Rows about a proposition are limited to the
propositions the token's owner is a member of; an owner sees every one. Rows
about the workspace itself are not about a proposition and are unaffected.

A document saves itself as it is typed, so one person writing one paragraph
would leave a row about every second. Once a run of those saves is two days old
it is folded into one row, the last of the run, whose `before` is the text the
block held when the run started: one row saying who changed what from what to
what. The rows it replaced are gone, so a cursor that finds ids missing between
two rows has not missed anything.

```json
{"activity": [
  {"id": 12, "proposition_id": 10, "actor_kind": "user", "actor_id": "1",
   "via": "token:research agent",
   "entity": "setting", "entity_id": "workspace.name", "action": "set",
   "before": "\"Workspace\"", "after": "\"Renamed workspace\"",
   "created_at": 1758067200}
]}
```

`actor_kind` is `user` or `system`, and `actor_id` is the user id. What a token
does is done by the person the token belongs to, so a change made through this
API or through MCP is recorded as theirs, with `via` naming what carried it:
`token:<name>` for this API and `mcp:<client>` for MCP. A change made in the
browser has no `via` and the field is absent. `before` and `after` are the JSON
of the entity before and after the change, and are absent when there was none.

## `GET /api/v1/propositions/{id}/events?since=&wait=`

Scope `read`, and the proposition has to be one the token's owner is a member
of. An owner reads every proposition; for everybody else a proposition they are
not a member of answers `404`, the same as one that is not there.

The stream of applied commands for one proposition, which is the same stream
the websocket carries and the same rows the activity log holds. `since` is the
sequence number of the last event you have seen, `0` or absent for the
beginning. With events waiting it answers at once. With none it holds the
request open for twenty five seconds and answers with the first that arrives,
or with an empty list if none does. `wait=0` answers at once either way, which
is what a client catching up after a reconnect asks for. An answer is capped at
200 events; ask again with the highest `seq` you were given.

```json
{"events": [
  {"seq": 41, "proposition": 10, "entity": "card", "entity_id": 7,
   "action": "move", "at": 1758067200,
   "actor": {"kind": "user", "id": 1, "name": "Ada Lovelace"},
   "before": {"id": 7, "column_id": 2, "position": "V", "title": "Call the engineer"},
   "after": {"id": 7, "column_id": 3, "position": "W", "title": "Call the engineer"}}
]}
```

`before` and `after` are the whole row as it was and as it is, so a client can
replace what it holds rather than patch it, and applying an event twice is
applying it once. `actor` is the person, with `via` naming the token or the MCP
client when one carried the change.

The browser does not use this path: it holds a websocket at `/ws`, and falls
back to `/api/events` with the session cookie it already has.

## `GET /api/v1/propositions/{id}/documents`

Scope `read`, and the proposition has to be one the token's owner is a member
of. An owner reads every proposition; for everybody else one they are not a
member of answers `404`, the same as one that is not there. Every document of
that proposition with the blocks still in it, in the order the tabs above the
document area stand.

```json
{"documents": [
  {"id": 4, "proposition_id": 10, "name": "Script", "slug": "script",
   "position": 1, "created_by": 1, "created_at": 1758067200, "revision": 12,
   "blocks": [
     {"id": 31, "document_id": 4, "position": "V", "text": "## Cold open",
      "version": 3, "updated_by": 1, "updated_at": 1758067200,
      "deleted_at": null}]}
]}
```

## `POST /api/v1/propositions/{id}/documents`

Scope `write`. The body is JSON with a `name`. The first document of a
proposition starts from the workspace's document template, with the
proposition's own statement in place of the placeholder; every one after it
starts from its own title. An empty name is `422`, as is a fifty first
document; a name another document there already has is given a numbered slug
rather than refused, and an archived proposition is `409`.

```
POST /api/v1/propositions/10/documents
{"name": "Interviews"}
```

The answer is the applied command, in the shape the event stream carries it, so
a client need not read the document back:

```json
{"event": {"seq": 88, "proposition": 10, "entity": "document", "entity_id": 7,
           "action": "create", "at": 1758067200,
           "actor": {"kind": "user", "id": 1, "name": "Ada Lovelace",
                     "via": "token:research agent"},
           "after": {"id": 7, "proposition_id": 10, "name": "Interviews",
                     "slug": "interviews", "position": 2, "created_by": 1,
                     "created_at": 1758067200, "revision": 0}}}
```

Every document and block write below answers the same way.

## `GET /api/v1/documents/{id}`

Scope `read`. One document, the blocks that are still in it, and `rendered`:
those blocks as the HTML the page draws.

```json
{"document": {"id": 4, "proposition_id": 10, "name": "Script",
  "slug": "script", "position": 1, "created_by": 1,
  "created_at": 1758067200, "revision": 12,
  "blocks": [{"id": 31, "document_id": 4, "position": "V",
              "text": "## Cold open", "version": 3, "updated_by": 1,
              "updated_at": 1758067200, "deleted_at": null}],
  "rendered": "<h2>Cold open</h2>"}}
```

## `PATCH /api/v1/documents/{id}`

Scope `write`. Renames it: the body is JSON with a `name`. An empty one is
`422`.

## `DELETE /api/v1/documents/{id}`

Scope `write`, and the standing to delete, which a researcher does not have. A
token whose owner may not delete is answered `404`, the same as one asking
about a document that is not there. The blocks and the revisions go with it.

## `GET /api/v1/documents/{id}/revisions`

Scope `read`. The snapshots kept of one document, newest first, fifty at most,
each the whole document as markdown. `reason` is `manual` for one somebody
asked for, `periodic` for the ten minute timer that runs while a document is
being edited, and `pre-import` for the one taken before markdown for the whole
document is read back in, whether the markdown watcher found it in a file or
`PUT /api/v1/documents/{id}/source` sent it.

```json
{"revisions": [
  {"id": 9, "document_id": 4, "markdown": "## Cold open\n\nTape first.",
   "created_by": 1, "created_at": 1758067200, "reason": "manual"}]}
```

## `POST /api/v1/documents/{id}/revisions`

Scope `write`. Keeps one now. A revision asked for over the API is `manual`,
which is what an empty `reason` means and the only one the body may name.
`periodic` belongs to the ten minute timer and `pre-import` to the markdown
watcher, so naming either here is `422`.

## `PUT /api/v1/documents/{id}/source`

Scope `write`. Writes a document, or the part of it `base` names, from
markdown. The body is the markdown and the blocks it stands for:

```json
{"base": [{"id": 31, "version": 4}, {"id": 32, "version": 1}],
 "text": "## Cold open\n\nTape first.\n\nThen the claim."}
```

The text is cut into paragraphs the way everything else is: at blank lines and
headings, except inside a fenced code block. Those paragraphs are lined up
against the text each base block held at the version named, and the difference
is written. A paragraph nobody touched keeps its block and is not written at
all, so its version does not move. A paragraph that changed is a set on the
block it came from, with the base version, so somebody else's change to that
block in the meantime is merged exactly as `PUT /api/v1/blocks/{id}` merges
one. A paragraph left over is a new block where it stands, in the order the
text has it. A block left over is deleted.

`base` is the blocks this text stands for, in the order the text has them, as
the caller last read them: from `GET /api/v1/documents/{id}`, or from the
answer to the last write of this same text. Left out altogether it is the
document as it stands when the request runs, which is what replacing a document
just read means; an empty list is a document with no blocks, so the two are not
the same thing.

A `base` naming only some of the document's blocks is the scope of the write:
those blocks are what the text stands for, and every other block is left
exactly where it is. That is how to rewrite one section without sending the
rest. The plain cost of it is that a paragraph of the text belonging to a block
outside the scope has no block to be matched to, so it is added.

The answer names the block each paragraph of the text now stands in, in the
order of the text, and lists the paragraphs that did not go in:

```json
{"base": [{"id": 31, "version": 5}, {"id": 32, "version": 7}, {"id": 44, "version": 1}],
 "conflicts": [{"block": 32, "version": 7, "current": "Then the counterclaim."}],
 "merged": [31]}
```

Send that `base` back with the same text and nothing is written: every
paragraph is already on the block it names, at the version it names. That is
what makes this safe to press twice, and it is how a client should follow one
write with another rather than reading the document again for every keystroke.

A block is in `conflicts` when somebody else changed it while the markdown was
being written and the two changes cannot be put together, or when the markdown
takes a paragraph out of a block somebody else has written in since. Either way
that block is left exactly as this server holds it and `current` is what it
holds, while the rest of the write goes in. The answer names it at the version
they left it at, so **writing the same text again under that base puts this
text's paragraph over theirs**: keeping yours is pressing again, and taking
theirs is reading the document again and working their paragraph into yours. A
paragraph this text leaves alone is different: the answer names it at the
version the text was written from, so a later write of it merges against what
they wrote rather than replacing it.

`merged` is the other half of `conflicts`: the blocks that took somebody else's
words in on the way, so what is stored there is neither what this text sent nor
what they wrote but both. Those paragraphs of the text the caller still holds
are out of date, and writing that text again would put its wording back over
the merge. To keep what came in, read the document again and write from that.
`conflicts` is what did not go in at all; `merged` is what went in changed.

Nothing is half applied: the whole thing is one transaction, and a revision
with reason `pre-import` is kept first, so a write that went wrong is one
restore away. A write that writes nothing keeps no revision.

A block somebody else added while the markdown was being written is not in
`base`, so the write says nothing about it and it stays where it is. A block
somebody else deleted keeps an unchanged paragraph out, so that writing an edit
made elsewhere does not put their deletion back, and puts a changed one in as a
new block where it stood.

`409` with no `conflict` object is a `base` naming a version whose text this
server no longer holds. The last twenty versions of every block are kept, and a
block still at the version `base` names needs none of them; past that there is
nothing to line the paragraphs up against, and lining them up wrongly would
move paragraphs between blocks, so the whole write is refused and nothing
changes. Read the document again and write that.

`422` is a write with more changed at once than the paragraphs can be placed
against. What is the same at the top and at the bottom of the document costs
nothing to line up, so this is a stretch of changed text long enough that
placing it would be a table of a million cells: roughly a thousand paragraphs
rewritten in one request. Send it in pieces.

The body may be a megabyte, rather than the sixty four kilobytes every other
body here is held to, because this one is a whole document. Past that it is
`413`.

`Idempotency-Key` is honored. A request sent again because its answer never
arrived is answered without applying anything a second time, and that answer
says so and carries no base:

```json
{"base": [], "conflicts": [], "merged": [], "replayed": true}
```

Nothing remembers what the first answer said, so read the document again before
writing any more of it.

## `POST /api/v1/documents/{id}/blocks`

Scope `write`. Inserts a block after the one `after` names. With `after` absent
or `0` the block goes in at the **head** of the document, above everything
already there. To put one at the end, send the id of the last block that
`GET /api/v1/documents/{id}` returned.

The MCP tool `append_block` is the other one: it reads the last block itself and
adds the paragraph at the **end**, which is why an agent writing a document a
call at a time reads in the order it wrote. The two are the same command
underneath and neither one changed; they differ only in what an absent `after`
means.

Text with a blank line in it arrives as one block per paragraph, in order, and
the answer is the command that made the first of them. A fenced code block is
one paragraph whatever is inside it, so code holding blank lines or lines
beginning with a hash is stored in one block rather than cut into several.

```
POST /api/v1/documents/4/blocks
{"after": 31, "text": "Tape from the hearing, then the number."}
```

`after_key` is the other way of saying where the block goes: the
`Idempotency-Key` an earlier call was sent under, meaning after the block that
call made. It is for a caller that has not read the answer to that call back
yet, which is what the browser is when it draws a block with no connection and
queues the two commands one behind the other. A call that made several blocks,
which is text with a blank line in it, is named by its one key and the new
block goes under the last of them.

```
POST /api/v1/documents/4/blocks
Idempotency-Key: 6b1dc7e0-1f2a-4c3b-9d4e-5a6b7c8d9e0f
{"after_key": "3f0a1b2c-4d5e-6f70-8192-a3b4c5d6e7f8", "text": "And the number."}
```

A key nobody spent, one spent by somebody else, and one whose call made no
block are all `404`, which is the answer an `after` that is not there gives
too. A key is forgotten after 24 hours, as above, and past that the call that
made the block is applied again rather than replayed, which records the key
afresh, so a sequence sent again a day later still lands in order. Sending
`after` and `after_key` together is `422`. The MCP tools take neither: neither
of them is told where to put the paragraph.

`whole` is optional and false by default, and means here what it means on
`PUT /api/v1/blocks/{id}` below: the text is stored exactly as it was sent,
edges and blank lines included, in exactly one block whatever it holds. It is
what the editor sends when Enter splits a block somebody is typing in, so that
the half paragraph under their caret arrives as they left it. Text longer than
a block may hold is `422`.

## `PUT /api/v1/blocks/{id}`

Scope `write`. Replaces one block's text. `base_version` is the version the
block was at when it was read, and what somebody else wrote since is merged
against it, so two people editing one block keep both edits. It is not
optional: a body without one is version zero, which no block is ever at, and
the answer is the conflict below.

A merge that cannot be made, and a `base_version` whose text this server no
longer holds, are both `409`:

```json
{"error": "that changed while you were editing it",
 "conflict": {"entity": "block", "entity_id": 31, "field": "text",
              "version": 5, "current": "The sea is a flywheel."}}
```

`current` is what the block holds now and `version` is the version it is at, so
the next attempt is that text with yours worked into it and that number as
`base_version`. Text longer than a block may hold is `422`.

Because the merge is measured from the text at `base_version`, this is also how
a caller takes a run of its own edits back: send the text the block held before
the run, with the `base_version` the block reached after it, and whatever
anybody wrote in another part of the block since is kept. A run that overlaps
what somebody else wrote, and one whose `base_version` is older than the texts
this server still holds, come back as the `409` above rather than being guessed
at.

`whole` is optional and false by default. Without it the text is trimmed and
cut into blocks at blank lines and headings, except inside a fenced code block,
where neither cuts, which is what a finished edit wants. With it the block
stores the text exactly as it was sent, which is what an editor saving every
few hundred milliseconds needs: trimming the blank line somebody is in the
middle of writing, or cutting the paragraph above the caret off into a block of
its own, is not something to do to a person as they type.
Everything else is the same, the merge included. A block saved this way keeps
what was sent, edge whitespace and blank lines included, until an ordinary set,
another route into this API, MCP or an import from the markdown mirror touches
it; each of those trims and cuts as it always has.

## `POST /api/v1/blocks/{id}/move`

Scope `write`. Puts the block after the one `after` names, or at the head when
it is `0` or absent. A move never conflicts with an edit: the two write
different columns, and the version guards only the text.

## `DELETE /api/v1/blocks/{id}`

Scope `write`. The block is tombstoned rather than removed: the row keeps its
text and its ordering key, so an undo is a column put back. Taking a paragraph
out of a document is writing the document, so this asks for the standing to
edit; deleting the document itself still asks for the standing to delete.

## The browser's own mount

The links, files and attachment routes below are mounted twice: under `/api/v1`
for a bearer token, and under `/app` for the browser, which carries a session
cookie and no token. `/app/links`, `/app/files/{id}/parts` and the rest are the
same handlers, the same bodies and the same statuses. A session has no scope,
so what a browser may do is the role of the person signed in, which every
command checks either way. The document and notification routes have no `/app`
twin: the browser writes documents over the websocket and reads their history
from `GET /documents/{id}/revisions`.

## `GET /api/v1/links?proposition=`

Scope `read`. The links saved on one proposition, newest first, each with the
citation built from its fields, and the kinds a link may be.

```json
{"links": [
  {"id": 12, "proposition_id": 10, "url": "https://example.com/report",
   "canonical_url": "https://example.com/report", "title": "The report",
   "author": "Ada Lovelace", "year": "2026", "kind": "paper",
   "note_md": "The table on page nine.", "question": "II", "added_by": 1,
   "created_at": 1758067200, "fetched_at": 1758067200,
   "citation": "Ada Lovelace (2026). The report. example.com"}],
 "kinds": ["article", "paper", "book", "essay", "video", "project",
           "dataset", "thread"]}
```

## `POST /api/v1/links`

Scope `write`. The body is JSON with a `proposition` and a `url`. The page is
read before the answer comes back, so the row already carries its title,
author, year and kind. Anything that is not an http or an https address is
`422`. The answer is the link row, as the list reports one.

```
POST /api/v1/links
{"proposition": 10, "url": "https://example.com/report"}
```

## `GET PATCH DELETE /api/v1/links/{id}`

Scope `read` to read one, `write` to change or remove it. A PATCH changes the
fields it names, `title`, `author`, `year`, `kind`, `note_md` and `question`,
and leaves the rest as they were. A kind outside the list, and a question that
is not `I`, `II`, `III`, `IV` or empty, are `422`. A DELETE answers
`{"deleted": true}`.

## `POST /api/v1/links/{id}/refetch`

Scope `write`. Reads the page again and answers with the link as it then
stands. What the fetch finds replaces what is on the row, which is why this is
a button and not a background job.

## `GET /api/v1/files?proposition=`

Scope `read`. The files of one proposition, newest first, and the folders a
file may be filed under. `state` is `uploading` until the object is in the
bucket and verified, and `ready` after that.

```json
{"files": [
  {"id": 8, "proposition_id": 10, "name": "hearing.wav",
   "folder": "Recordings", "kind": "wav", "size": 734003200,
   "object_key": "10-student-debt-is-a-policy-choice/8/hearing.wav",
   "version_of": null, "duration_ms": 5400000, "width": null, "height": null,
   "uploaded_by": 1, "state": "ready", "created_at": 1758067200}],
 "folders": ["Documents", "Reading", "Recordings", "Art"]}
```

## `POST /api/v1/files`

Scope `files`. Records a file and hands back the way to put the object in the
bucket: the app never receives the bytes. The body names the `proposition`, the
`name`, the `folder`, the `size` in bytes, and optionally `replace`, the id of
a file this one is a new version of.

Up to 64 MiB the answer carries one presigned PUT and the headers to send with
it:

```json
{"file": {"id": 9, "proposition_id": 10, "name": "tides.md",
          "folder": "Documents", "kind": "md", "size": 12,
          "object_key": "10-student-debt-is-a-policy-choice/9/tides.md",
          "version_of": null, "duration_ms": null, "width": null,
          "height": null, "uploaded_by": 1, "state": "uploading",
          "created_at": 1758067200},
 "url": "https://example.com/theses/10-student-debt-is-a-policy-choice/9/tides.md?...",
 "headers": {"Content-Type": "text/markdown; charset=utf-8"},
 "expires_at": 1758070800, "ttl_seconds": 3600}
```

Anything larger is a multipart upload, with an `upload_id`, the `part_size`,
and a batch of sixty four presigned part URLs:

```json
{"file": {"id": 8, "name": "hearing.wav", "size": 734003200,
          "state": "uploading"},
 "upload_id": 3, "part_size": 67108864,
 "parts": [{"number": 1, "url": "https://example.com/theses/...&partNumber=1"}],
 "expires_at": 1758070800, "ttl_seconds": 3600}
```

A `done` list comes with it once the bucket already holds parts, which is the
resume; on a first answer there are none and it is left out.

`expires_at` is when those URLs stop working by this server's clock, and
`ttl_seconds` is how long they last from the moment the answer arrives, which
is what a client counts from, since its own clock may be minutes out. A folder
that is not one of the four, a name that is empty once it has been cleaned of
paths and control characters, and a size of no bytes or of more than the
largest object allowed, are `422`; object storage nobody has set up yet is
`503`. An object is ten thousand parts of 64 MiB at most.

## `GET /api/v1/files/{id}/parts?after=`

Scope `files`. The resume, for a client that reloaded and knows only its file
id. It answers with the part numbers the bucket already holds, in `done`, and
freshly signed URLs for the next batch of the ones it does not, starting after
`after`. Asking is also the sign that somebody is still uploading: the 48 hours
the sweep abandons an upload after are 48 hours of silence, and every batch
pushes them out again.

A part number past the end of the upload is `422`, and so is a file that is
already `ready`, because the upload is over. A negative one is the beginning.
A file small enough for one PUT returns a fresh `url` and `headers`, with the
same shape as its creation response. The client sends the whole file again.

## `POST /api/v1/files/{id}/complete`

Scope `files`. Called once the last byte is in the bucket. The server assembles
the parts, checks the object is there and is exactly the size the upload
declared, renders a thumbnail if it is an image it reads, and only then marks
the file `ready`. The body may carry `duration_ms`, `width` and `height` as the
client measured them; an image the server rendered a thumbnail for reports its
own dimensions instead.

Object keys are opaque. New uploads include a random path component; completing
a file up to 64 MiB copies it to a separate final key before marking it ready.
Clients must use the returned row and download endpoint rather than retaining
the upload key as the completed object's location.

```json
{"file": {"id": 9, "name": "tides.md", "state": "ready", "size": 12}}
```

A completion sent before every part arrived, a second completion, and one for
an upload the sweep has already abandoned are all `422`. So is an object that
is not the size the upload declared, and that one is thrown away with its row,
so the same file can be added again rather than sitting at `uploading` forever.

## `GET /api/v1/files/{id}/download`

Scope `read`. A presigned GET that lasts fifteen minutes, with the file's
current name on it so a browser saves it under that rather than under its
object key. A file that is not `ready` is `422`.

```json
{"url": "https://example.com/theses/10-student-debt-is-a-policy-choice/9/tides.md?..."}
```

## `GET /api/v1/files/{id}/thumb`

Scope `read`. The same for the thumbnail rendered beside the original when the
file was completed. A file that has none is `404`.

## `GET /api/v1/files/{id}/versions`

Scope `read`. The chain under one file, newest first: what it replaced, what
that replaced, and so on. The file itself is not in the list, and a file that
replaced nothing answers an empty one.

```json
{"versions": [
  {"id": 6, "proposition_id": 10, "name": "tides.md", "folder": "Documents",
   "kind": "md", "size": 11,
   "object_key": "10-student-debt-is-a-policy-choice/6/tides.md",
   "version_of": null, "duration_ms": null, "width": null, "height": null,
   "uploaded_by": 1, "state": "ready", "created_at": 1757980800}]}
```

## `PATCH /api/v1/files/{id}`

Scope `write`. Renames a file or moves it to another folder, whichever of
`name` and `folder` the body carries, and leaves the other as it was. The
object keeps the key it was written under, so a link already handed out goes on
working. A move between folders that live in different buckets is `422`:
download it and upload it again.

## `DELETE /api/v1/files/{id}`

Scope `files`. Removes the row, then the object, its thumbnail and any
multipart upload still in flight. Answers `{"deleted": true}`.

## `GET /api/v1/attachments?proposition=`

Scope `read`. What hangs off the cards of one proposition, which is what the
board draws on the cards themselves.

```json
{"links": [{"card_id": 7, "link_id": 12}],
 "files": [{"card_id": 7, "file_id": 8}]}
```

## `POST DELETE /api/v1/cards/{card}/links/{id}`

Scope `write`. Attaches a link to a card, or takes it off again. The card and
the link have to be on the same proposition; anything else is `404`. Attaching
twice leaves one attachment.

```json
{"card": 7, "action": "attach"}
```

## `POST DELETE /api/v1/cards/{card}/files/{id}`

Scope `write`. The same for a file.

## `GET /api/v1/me/notifications`

Scope `read`. The channels the token's owner has, every event they can be told
about, and which channels are ticked for each one. No secret is in it: an ntfy
token comes back as `token_set` and a webhook secret as `secret_set`, and a
Pushover channel's label carries the last four characters of its user key,
which is what tells two of them apart and nothing else.

```json
{"channels": [
  {"id": 2, "kind": "ntfy", "label": "ntfy.sh/ada-theses", "verified": true,
   "quiet_from": "23:00", "quiet_to": "07:00", "digest": false,
   "server": "https://ntfy.sh", "topic": "ada-theses", "token_set": true}],
 "events": [{"key": "assigned", "label": "Assigned to a card"},
            {"key": "mentioned",
             "label": "Mentioned in a note, a description or a document"}],
 "rules": {"mentioned": [2]}}
```

## `PUT /api/v1/me/notifications`

Scope `write`. The body carries `channels`, `rules`, or both, and replaces what
it carries. Channels are a whole list, so one this account has that the list
leaves out is deleted. A secret left out keeps the stored one and an empty
string clears it. A channel arrives unverified and stays silent until a test
reaches it, and one edited to point somewhere else is unverified again. Email
is the exception: it goes to the address the account signs in with, so it is
verified the moment it is saved and there is nothing for a test to prove.

```
PUT /api/v1/me/notifications
{"channels": [{"kind": "ntfy", "topic": "ada-theses", "token": "tk_...",
               "quiet_from": "23:00", "quiet_to": "07:00", "digest": false}],
 "rules": {"mentioned": [2]}}
```

A channel that cannot work, quiet hours that are not two times of day, and an
event key the matrix does not hold are `422`. An `id` this account does not own
is `404`. The answer is what `GET` reports.

Quiet hours hold a message until they end, and a channel set to `digest` holds
everything until the digest time set on `/settings`. Being named is the
exception: a mention arrives at once, on the first channel that was going to
get it at all.

## `POST /api/v1/me/notifications/test`

Scope `write`. The body is JSON with a `channel`. Sends one message to it now,
outside the outbox, and marks it verified if it arrives. A channel belonging to
somebody else, or to the workspace, is `404`. A destination that refused is
`502`, with what it said.

```json
{"sent": true, "channel": 2}
```

## `GET /api/v1/settings`

Scope `admin`. Every known setting, its definition and its current value.

```json
{"settings": [
  {"key": "workspace.name", "kind": "string", "label": "Name", "set": true,
   "value": "Renamed workspace"},
  {"key": "workspace.release_day", "kind": "choice", "label": "Release day",
   "choices": ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday",
               "Saturday", "Sunday"], "set": false, "value": "Monday"},
  {"key": "mail.password", "kind": "string", "label": "Password",
   "secret": true, "set": true}
]}
```

`kind` is `string`, `int`, `list`, `choice` or `text`. A secret is reported as
`"secret": true` with `"set"` saying whether one is stored; its value is never
returned and cannot be read back through the API.

## `PUT /api/v1/settings/{key}`

Scope `admin`. The body is JSON with a `value`: a string, a number or, for a
`list` setting, an array of strings. The value is validated against the key's
definition, which is what a bad number, an unknown choice or an empty list is
refused for. The response is the setting as `GET /api/v1/settings` reports it.

```
PUT /api/v1/settings/signin.session_days
{"value": 7}
```

A value the key's definition refuses is `422`; a body that is not JSON, or has
no `value` field, is `400`.

A write is recorded in the activity log as the person the token belongs to.

## The board

Propositions, columns, cards, checklist items and notes are the same commands
the app sends over its websocket, so a change made here appears on an open
board at once and is recorded in the activity log with the token that carried
it.

Two rules run through all of them. A proposition the token's owner is not a
member of answers `404` on every route, the same as one that is not there; an
owner is a member of every proposition. An archived proposition is read only,
and a write to one answers `409`; restoring it and deleting it are the writes
that still work.

## `GET /api/v1/propositions`

Scope `read`. The rail: every proposition the token's owner may read, in its
own order, archived ones included and carrying their `archived_at`. It is
filtered rather than refused, so a member of nothing reads an empty list.

```json
{"propositions": [
  {"id": 10, "number": 1, "title": "Tidal Power", "statement": "The tide is a battery.",
   "blurb": "", "status": "idea", "episode": null, "target_date": null,
   "position": "V", "created_at": 1758067200, "archived_at": null, "members": [1]}
]}
```

## `POST /api/v1/propositions`

Scope `write`. Takes `title`. The proposition starts with the workspace's
default status and columns, and the token's owner is its first member. A title
that is empty or too long is `422`. The answer is the applied event, whose
`after` is the whole row.

```
POST /api/v1/propositions
{"title": "Tidal Power"}
```

```json
{"event": {"seq": 12, "proposition": 10, "entity": "proposition", "entity_id": 10,
  "action": "create", "at": 1758067200,
  "actor": {"kind": "user", "id": 1, "name": "Nora Vance", "via": "token:research agent"},
  "after": {"id": 10, "number": 1, "title": "Tidal Power", "status": "idea", "members": [1]}}}
```

## `GET /api/v1/propositions/{id}`

Scope `read`. One proposition with its members.

```json
{"proposition": {"id": 10, "number": 1, "title": "Tidal Power", "status": "idea",
  "episode": null, "target_date": null, "archived_at": null, "members": [1]}}
```

## `PATCH /api/v1/propositions/{id}`

Scope `write`. Changes the fields the body names: `title`, `statement`,
`blurb`, `status`, `episode`, `target_date`. A field left out keeps what is
there; a field sent empty clears it. A body that names none of them is `400`.
`status` has to be one of the workspace's statuses, which `GET
/api/v1/settings` reports as `defaults.statuses`; a word that is not on the
list is `422`, because the rail groups by status and would have nowhere to draw
the proposition.
The fields land as one transaction, so a refusal partway through leaves
nothing behind.

```
PATCH /api/v1/propositions/10
{"status": "recording", "episode": "12"}
```

## `POST /api/v1/propositions/{id}/archive`

Scope `write`. Puts the proposition away. Everything on it is read only until
it is restored.

```json
{"event": {"entity": "proposition", "entity_id": 10, "action": "archive",
  "after": {"id": 10, "archived_at": 1758067200}}}
```

## `POST /api/v1/propositions/{id}/restore`

Scope `write`. Takes it back out. This is the one write an archived
proposition accepts.

## `POST /api/v1/propositions/{id}/move`

Scope `write`. Takes `after`, the id of the proposition to sit behind in the
rail, or `0` for the head of it. An `after` that is not a proposition is
`404`.

```
POST /api/v1/propositions/10/move
{"after": 11}
```

## `DELETE /api/v1/propositions/{id}`

Scope `write`, and the role has to be one that may delete, which a researcher
is not. A token whose owner may not delete is answered `404`, the same as a
proposition that is not there. The board, the documents, the links and the
files go with it, and the record of the deletion is filed with no proposition
so that it survives the cascade. An archived proposition can be deleted without
being restored first: deleting and restoring are the two writes an archived one
still takes.

## `POST /api/v1/propositions/{id}/members/{user}`

Scope `write`. Puts somebody on the proposition, which is what lets them read
it at all. Somebody who is not in the workspace is `404`. Adding somebody
already on it changes nothing and answers `200`.

```
POST /api/v1/propositions/10/members/2
```

## `DELETE /api/v1/propositions/{id}/members/{user}`

Scope `write`. Takes them off again, after which the proposition answers `404`
to their tokens. Somebody who was not on it is `404`, so a removal that answers
`200` is one that happened.

## `GET /api/v1/propositions/{id}/columns`

Scope `read`. The columns of one proposition in their order.

```json
{"columns": [{"id": 2, "proposition_id": 10, "name": "Research", "position": "V"}]}
```

## `POST /api/v1/propositions/{id}/columns`

Scope `write`. Takes `name` and puts the column at the end. A name that is
empty or too long is `422`.

## `PATCH /api/v1/columns/{id}`

Scope `write`. Takes `name` and renames the column.

## `POST /api/v1/columns/{id}/move`

Scope `write`. Takes `after`, the column to sit behind, or `0` for the head.

## `DELETE /api/v1/columns/{id}`

Scope `write`, and the role has to be one that may delete. A column with cards
still in it is `409`: the cascade would take them without the caller being
told. Move them out first.

```json
{"error": "move the cards out of that column first"}
```

## `GET /api/v1/propositions/{id}/cards`

Scope `read`. Every card on the proposition, each one whole, with the sequence
number this reading is of so a client can follow the event stream on from it.

```json
{"cards": [
  {"id": 7, "proposition_id": 10, "column_id": 2, "position": "V",
   "title": "Call the engineer", "description_md": "", "question": null,
   "due_date": null, "done_at": null, "created_at": 1758067200, "version": 1,
   "assignees": [1], "checklist": [], "comments": []}
], "seq": 41}
```

## `GET /api/v1/cards/{id}`

Scope `read`. One card with its assignees, checklist and notes.

## `POST /api/v1/columns/{id}/cards`

Scope `write`. Takes `title` and an optional `assignees`, a list of user ids.
The card goes at the end of the column. A column that is not there, or is on a
proposition the token's owner may not read, is `404`.

```
POST /api/v1/columns/2/cards
{"title": "Call the engineer", "assignees": [1]}
```

## `PATCH /api/v1/cards/{id}`

Scope `write`. Changes the fields the body names: `title`, `description_md`,
`question`, `due_date`. A body that names none of them is `400`.

`title` and `description_md` are versioned, and the body carries the
`base_version` the editor started from. A card has one version across both, so
a body naming both sends the second change the version the first one left. An
edit that began before somebody else's is `409` with the text the card holds
now, and nothing of that body lands.

`question` is one of `I`, `II`, `III` or `IV`, or empty for none; anything
else is `422`.

`due_date` is a calendar day written `YYYY-MM-DD`, or empty to clear it. The
board asks of it only whether that day has gone, so anything else, a month
name, a day the month does not have, a phrase, is `422`.

```
PATCH /api/v1/cards/7
{"base_version": 1, "title": "Call the harbor engineer"}
```

```json
{"error": "that changed while you were editing it",
 "conflict": {"entity": "card", "entity_id": 7, "field": "title",
              "version": 2, "current": "Call the pilot"}}
```

## `POST /api/v1/cards/{id}/move`

Scope `write`. Takes `column` and `after`. A body that names no column is
`400`. The column has to be on the same proposition, and one that is not is
`404`. `after` is the card to sit behind,
or `0` for the head of the column. Moves never conflict; the last one wins by
server order.

```
POST /api/v1/cards/7/move
{"column": 3, "after": 0}
```

## `POST /api/v1/cards/{card}/assignees/{user}`

Scope `write`. Puts somebody on the card. A person who is not in the workspace
is `404`. Assigning somebody already on it changes nothing and answers `200`.

## `DELETE /api/v1/cards/{card}/assignees/{user}`

Scope `write`. Takes them off again.

## `POST /api/v1/cards/{id}/done`

Scope `write`. Marks the card done. It takes no body.

## `POST /api/v1/cards/{id}/reopen`

Scope `write`. Clears it again.

## `DELETE /api/v1/cards/{id}`

Scope `write`, and the role has to be one that may delete.

## `POST /api/v1/cards/{id}/checklist`

Scope `write`. Takes `text` and adds an item at the end of the card's
checklist.

```json
{"event": {"entity": "checklist_item", "entity_id": 4, "action": "create",
  "after": {"id": 4, "card_id": 7, "text": "Ring the harbor",
            "done": false, "position": "V"}}}
```

## `PATCH /api/v1/checklist/{id}`

Scope `write`. Takes `done`, a boolean. A body without it is `400`.

## `DELETE /api/v1/checklist/{id}`

Scope `write`.

## `POST /api/v1/cards/{id}/comments`

Scope `write`. Takes `body_md`, the markdown of the note. An `@handle` in it
mentions that person, which reaches their notifications.

```
POST /api/v1/cards/7/comments
{"body_md": "@ada they answered."}
```

## `DELETE /api/v1/comments/{id}`

Scope `write`. Only the note's own author may delete it, whatever the role:
somebody else's is `403`. The activity log is a record, not a wall to
moderate.

```json
{"error": "that is not yours to delete"}
```

## `POST /api/v1/activity/{id}/undo`

Scope `write`. Puts back the `before` of one activity row and marks the row
undone. The undo is itself a command: it is authorized, recorded and published
like any other edit, so an open board sees it happen.

It is `409` when the change cannot be put back: a create, a delete that took
the row away, a row already undone, a change that moved nothing undo can
write, or an entity that has moved on since, which comes back as the same
conflict a stale edit does. A row about a proposition the token's owner may
not read is `404`. These are the refusals the websocket's undo gives, because
they are the same function.

```json
{"event": {"entity": "card", "entity_id": 7, "action": "undo",
  "before": {"id": 7, "title": "Call the pilot"},
  "after": {"id": 7, "title": "Call the engineer"}}}
```

```json
{"error": "that change cannot be undone"}
```

## MCP

`/mcp` is a streamable HTTP MCP endpoint. It takes the same
`Authorization: Bearer` header and the same scopes, and it is stateless: every
POST is authenticated on its own. Only POST is served; GET and DELETE are 405. A
POST body over 64 KiB is refused with 413, as on `/api/v1`. A token that the
tool's scope does not cover is a tool error rather than an HTTP status, since
one endpoint serves every tool.

| Tool | Scope | What it does |
| --- | --- | --- |
| `whoami` | any | Reports the token this connection is using and the person it belongs to. |
| `search` | `read` | Searches cards, documents, links, files, comments, propositions and people. |
| `list_users` | `read` | Lists everyone in the workspace. |
| `get_settings` | `admin` | Lists the workspace settings, with secrets reported as set rather than returned. |
| `set_setting` | `admin` | Changes one workspace setting. |
| `list_documents` | `read` | Lists the documents of one proposition and how many blocks each holds. |
| `read_document` | `read` | Reads one document as markdown, with each block's id and version. |
| `create_document` | `write` | Creates a document in a proposition and returns its id. |
| `append_block` | `write` | Adds a paragraph at the end of a document, unlike `POST /api/v1/documents/{id}/blocks` with no `after`, which puts one at the head. |
| `insert_after_heading` | `write` | Adds a paragraph at the end of the section under a heading. |
| `replace_block` | `write` | Replaces the text of one block. |
| `move_block` | `write` | Moves one block within its document, after another block or to the head of it. |
| `write_document` | `write` | Replaces a document, or the part of it `base` names, with markdown, and answers with the block each paragraph now stands in, what did not go in and what went in changed: `PUT /api/v1/documents/{id}/source` with `base` and `key` optional in the same way. |
| `list_links` | `read` | Lists the links saved on one proposition, with their citation. |
| `add_link` | `write` | Saves a URL on one proposition, reading the page for its title, author, year and kind. |
| `annotate_link` | `write` | Changes a saved link's note, kind and question. |
| `list_files` | `read` | Lists the files uploaded to one proposition, with their folder, size and state. |
| `get_download_url` | `read` | Returns a download link for one file that works for a few minutes. |
| `request_upload` | `files` | Makes a file row and returns its presigned URLs. |
| `attach_to_card` | `write` | Attaches a link or a file to a card on the same proposition, or detaches it. |
| `list_propositions` | `read` | Lists the propositions this token's owner may read. |
| `get_proposition` | `read` | Reads one proposition and its schedule. |
| `create_proposition` | `write` | Starts a proposition with the default columns. |
| `set_status` | `write` | Moves one proposition to one of the workspace's statuses. |
| `list_cards` | `read` | Lists the columns and cards of one proposition, with the sequence number. |
| `create_card` | `write` | Adds a card at the end of a column. |
| `move_card` | `write` | Moves a card into a column on the same proposition. |
| `assign_card` | `write` | Puts somebody on a card, or takes them off. |
| `complete_card` | `write` | Marks a card done, or reopens it. |
| `comment` | `write` | Writes a note on a card. |
| `activity` | `read` | Reads the activity log after a sequence number. |
| `backup_now` | `admin` | Starts one backup in the background. |

Every tool returns structured output against a schema the tool list carries, and
is annotated with what calling it does. The read tools are read only.
`set_setting`, `replace_block`, `move_block`, `set_status`, `move_card`,
`complete_card` and `annotate_link` are destructive and idempotent, since each
replaces what was there. `create_document`, `append_block`,
`insert_after_heading`, `add_link`, `create_proposition`, `create_card`,
`comment`, `request_upload` and `backup_now` are neither, because calling one
twice makes two of the thing, and `add_link` also reads a page on the open web.
`attach_to_card` and `assign_card` are idempotent without being destructive:
doing either twice leaves the one thing there.

The tools that make something take an optional `key`, which is the same idea as
the `Idempotency-Key` header above and takes the same 1 to 64 letters, digits,
hyphens or underscores. A call an agent never saw the answer to is made again
under the same key and returns the id of the thing the first call made rather
than making a second: `create_proposition`, `create_card`, `comment`,
`create_document`, `append_block`, `insert_after_heading` and `add_link`. The
tools that set a field take none, because setting it twice sets it to what it
already holds.

`replace_block` takes the `base_version` that `read_document` reported and
merges in what somebody else wrote since; left out, it reads the block and
writes over whatever it holds. A merge that cannot be made is a tool error
carrying the version the block is now at and the text it holds, so the next
call is that text with yours worked into it.

A tool reads and writes only what its token's owner may: a proposition they are
not a member of answers the same way as one that is not there, an archived
proposition refuses every write, and a card cannot move to a column on another
proposition. `backup_now` answers as soon as the archive has begun; what it did
is read from the settings page.

Resource `theses://workspace` describes the workspace: its name, time zone, how
many people and propositions it holds, and what this endpoint can do. Three
templates read one proposition: `theses://proposition/{id}` is its status,
schedule and board as text, `theses://proposition/{id}/document/{slug}` is one
of its documents as markdown, and `theses://proposition/{id}/links` is its links
with their citations, notes and questions.

The server speaks protocol revisions `2026-07-28` back to `2024-11-05` and
settles on the newest the client offers. At `2026-07-28` a client names itself on
every call, so a write is attributed to that client; an older client does not,
and the token's name is used instead.

A write through MCP is recorded in the activity log as the person the token
belongs to, with `mcp:<client>` in the log's `via` field.

## What is not here

Adding a file from Drive and publishing to Transistor are browser routes only.
Neither has an API or MCP surface yet.
