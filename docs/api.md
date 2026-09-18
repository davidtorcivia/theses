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
route says which scope it needs; a token without it is refused.

| Status | When |
| --- | --- |
| 401 | no `Authorization: Bearer` header, or the token is unknown or revoked |
| 403 | the token is valid but does not have the scope the route needs |
| 404 | no such endpoint, or no such settings key |
| 413 | the request body is over 64 KiB |
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

Anything in `q` that is not a letter or a digit is punctuation to match, not
syntax: quotation marks, asterisks, `NEAR`, `AND`, `OR` and `NOT` are searched
for as words. The last word is treated as a prefix, so `deb` finds `debt`. A
query with no letter or digit in it returns no groups.

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
   "before": "\"We All Fall Down\"", "after": "\"Debt Machine\"",
   "created_at": 1758067200}
]}
```

`actor_kind` is `user` or `system`, and `actor_id` is the user id. What a token
does is done by the person the token belongs to, so a change made through this
API or through MCP is recorded as theirs. A `via` field naming the token or the
MCP client that carried it will be added to these rows. `before` and `after` are
the JSON of the entity before and after the change, and are absent when there
was none.

## `GET /api/v1/settings`

Scope `admin`. Every known setting, its definition and its current value.

```json
{"settings": [
  {"key": "workspace.name", "kind": "string", "label": "Name", "set": true,
   "value": "Debt Machine"},
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
POST is authenticated on its own.

| Tool | Scope | What it does |
| --- | --- | --- |
| `whoami` | any | Reports the token this connection is using and the person it belongs to. |
| `search` | `read` | Searches cards, documents, links, files, comments, propositions and people. |
| `list_users` | `read` | Lists everyone in the workspace. |
| `get_settings` | `admin` | Lists the workspace settings, with secrets reported as set rather than returned. |
| `set_setting` | `admin` | Changes one workspace setting. |

Resource `theses://workspace` describes the workspace: its name, time zone, how
many people and propositions it holds, and what this endpoint can do.

A write through MCP is recorded in the activity log as the person the token
belongs to.
