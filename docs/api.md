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

| Status | When |
| --- | --- |
| 401 | no `Authorization: Bearer` header, or the token is unknown or revoked |
| 403 | the token does not have the scope the route needs, or the person it belongs to no longer has the standing that scope implies |
| 404 | no such endpoint, or no such settings key |
| 413 | the request body is over 64 KiB, which `/api/v1` and `/mcp` both allow |
| 429 | over 300 requests a minute for one token |
| 500 | a fault on the server; the detail is in its log, not in the response |

Every refusal is JSON with one field:

```json
{"error": "this token does not have the admin scope"}
```

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
10 by default and 50 at most.

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
with the last id you were given.

```json
{"activity": [
  {"id": 12, "proposition_id": 10, "actor_kind": "user", "actor_id": "1",
   "entity": "setting", "entity_id": "workspace.name", "action": "set",
   "before": "\"Workspace\"", "after": "\"Renamed workspace\"",
   "created_at": 1758067200}
]}
```

`actor_kind` is `user` or `system`, and `actor_id` is the user id. What a token
does is done by the person the token belongs to, so a change made through this
API or through MCP is recorded as theirs. A `via` field naming the token or the
MCP client that carried it will be added to these rows. `before` and `after` are
the JSON of the entity before and after the change, and are absent when there
was none.

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
is what a client catching up after a reconnect asks for.

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

A write is recorded in the activity log as the person the token belongs to.

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

Every tool returns structured output against a schema the tool list carries, and
is annotated with whether it only reads: the four read tools are read only and
`set_setting` is marked destructive and idempotent, since it replaces a value
that was there.

Resource `theses://workspace` describes the workspace: its name, time zone, how
many people and propositions it holds, and what this endpoint can do.

The server speaks protocol revisions `2026-07-28` back to `2024-11-05` and
settles on the newest the client offers. At `2026-07-28` a client names itself on
every call, so a write is attributed to that client; an older client does not,
and the token's name is used instead.

A write through MCP is recorded in the activity log as the person the token
belongs to.
