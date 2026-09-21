# Application audit, 2026-09-20

The audit covered the Go services, SQLite schema and migrations, HTTP and MCP
surfaces, browser state and offline recovery, uploads, notifications, document
mirrors, authentication, and keyboard interaction. The fixes retain the existing
single-process architecture and add no runtime dependencies.

## Security and account integrity

| Defect | Change and regression coverage |
| --- | --- |
| Request and rejection logs included reset and invitation credentials. | Log the route pattern or redact the credential before routing. Tests visit real token URLs and reject requests before routing. |
| Notification destinations used a separate, incomplete public-address filter. | Webhooks and ntfy use the existing DNS-pinning `safehttp` client. Tests reject private, shared, NAT64 and 6to4 destinations and redirects. |
| Replayed commands could return an old proposition's data after membership changed. | Reauthorize the original event's proposition; detached deletion replays require owner standing. Tests remove membership and demote an owner before replay. |
| A dropped membership event or reused proposition ID could leave a socket with stale access. | Disconnect on bus overflow and discard membership on deletion. Reconnection rebuilds authorization. |
| Password reset could spend the token before all related writes succeeded. | Token claim, password change, activity and session invalidation share one transaction. An injected audit failure verifies rollback. |
| API-token creation could commit a credential whose audit write failed. | Credential creation and activity share one transaction. A failure test verifies no active token remains. |
| Browser, API and MCP callers could alter scheduler-maintained settings. | Internal settings accept only the system actor and are omitted from public settings descriptions. |
| Email lookup was case-insensitive but database uniqueness was case-sensitive; only one blank email was allowed. | Migration 005 enforces uniqueness for nonempty lowercased emails. Tests cover multiple blank emails, case collisions, migration rollback and retained foreign keys. Profile and invitation collisions produce useful responses. |

## Correctness and recovery

| Defect | Change and regression coverage |
| --- | --- |
| Settings forms could save early fields before rejecting a later field. | Validate the complete batch, commit settings and activity together, then update the cache. |
| Settings writes could hold the cache lock while waiting for a transaction that itself needed settings. | Serialize writers separately and acquire the cache lock only for the post-commit update. A blocked-writer regression verifies reads remain available. |
| Concurrent commands could publish events in a different order from their commits. | Serialize commit and bus publication. Concurrent command tests verify ascending event sequence. |
| Browser acknowledgments could advance the catch-up cursor past missed events, and old payloads could overwrite newer rows. | Keep a separate paginated stream cursor and compare event sequence per entity, including embedded children. Tests interleave live events with multiple catch-up pages. |
| A material-list response could replace changes received while the request was in flight. | Reapply newer file, link and attachment events after loading the response. |
| A failed WebSocket prevented commands even when HTTP worked. | Share command execution through authenticated, CSRF-protected `POST /app/commands`; long-poll events and replay queued commands with their original keys. Tests include an empty workspace. Presence still requires WebSockets. |
| Link and file edits sent stale copies of unrelated fields. | Patch only supplied fields inside the command transaction across browser, API and MCP callers. |
| MCP link creation could leave a link behind when its annotations were invalid. | Validate and insert the link, note and question as one command. Tests verify invalid annotations leave no link and keyed retries retain the original result. |
| Deleted propositions left markdown mirrors behind; dropped events could leave obsolete mirrors attached to reused rows. | Remove owned mirrors on deletion and reconcile against current document identity, creation event and path after overflow. Preserve hand edits without importing them into a replacement document. |
| A failed read of an existing markdown mirror could permit overwriting unreadable hand edits. | Refuse ordinary mirror writes on read errors other than a missing file. |
| Notification schedules shifted by an hour across daylight-saving transitions. | Construct the requested local clock time directly. Tests cover both transition dates and malformed signed times. |
| Simultaneous publication requests could create duplicate external episodes. | Permit one publication at a time and refuse the second request before calling the provider. |
| Archived propositions offered an Undo action that the command would reject. | Exclude archive events from the advertised undoable actions. |

## Files and uploads

| Defect | Change and regression coverage |
| --- | --- |
| Upload replay trusted the original event rather than current file state, access and storage location. | Read the current row and permissions; refuse ready or deleted files and use the original stored size. |
| Concurrent retries could start multiple multipart uploads for one command. | Serialize upload setup and abort an unrecorded multipart upload if its database insert fails. |
| Multipart assembly could succeed while the response or subsequent database write failed. | Check for the assembled object before attempting to reuse the consumed multipart upload. |
| A still-valid single-PUT URL could overwrite a completed object. | Copy uploads up to 64 MiB to a unique final key before marking them ready. Old URLs name only the prior object. New upload keys also include randomness so deleted IDs cannot reuse an old writable path. |
| Stale failed-upload cleanup could delete a file completed by another request. | Delete only a row that remains uploading under the original object key. |
| Import retries could overwrite completed bytes. | Return the current ready file before streaming again. |
| Folders with equal bucket names on different providers were treated as the same storage. | Compare endpoint and region as well as bucket name before allowing a metadata-only move. |
| Small uploads could not resume after reload. | Return a fresh single-PUT URL from the parts endpoint. |
| Saving an entire browser File in IndexedDB could exhaust quota before upload. | Persist small progress metadata and keep the File in the current tab. After reload, reselect and validate the original file. Keep queued metadata and its key until the server upload has been recorded durably. |

Single-PUT completion adds a server-side copy and cleanup for files up to 64 MiB.
It uses the existing storage client and does not transfer the file through the
application process. A late PUT can recreate the old, unreferenced object until
its URL expires; it cannot change the ready file. A process crash between copy
and commit can also leave an unreferenced object. No automatic bucket-wide
garbage collection is added.

## UX and accessibility

- Preserve source markdown when retrying a save whose first response was lost,
  including wording that may have conflicted. Warn before leaving unsaved text.
- Keep card text conflicts in the existing outbox. If browser storage fails,
  retain the local choice and guard navigation until it is answered.
- Support keyboard activation of file and link rows and editable fields, focus
  transfer into drawers, and focus restoration when closing them.
- Add listbox selection semantics and keyboard navigation to assignment and
  mention pickers.
- Open the actual card from a file or link's "Used in" reference.
- Scroll the nearest scrolling container during touch dragging.
- Re-enable Undo after a refused attempt.

## Validation and limits

Validation includes formatting, `go vet ./...`, the full Go test suite, the full
race-enabled Go suite, JavaScript module syntax checks, all browser-module Node
tests, and a production build with CGO disabled. CI runs all Node regression
files rather than a fixed subset.

`govulncheck` found no reachable vulnerable code and no imported vulnerable
packages. Its module scan reported GO-2026-5932 in the unused
`golang.org/x/crypto/openpgp` package. The repository has no Go benchmarks; no
latency or throughput improvement is claimed. Upload setup and publication
remain serialized, and UI state tests use simulated browser globals.

Rendered desktop/mobile and assistive-technology behavior could not be tested:
the audit session had no available browser. External providers were exercised
through existing local test servers, not live accounts. These results do not
certify deployment configuration or provider compatibility beyond those tests.

Source drafts are not durable after a user explicitly leaves the page. Direct
file/link metadata edits, deletes and attachment requests require a connection;
the existing command outbox does not cover every HTTP mutation. Browser upload
resumption after reload requires choosing the original file. Imports retain the
existing 5 GiB limit.

Notification matching retains its existing best-effort, in-process event-bus
contract: a crash before matching or bus overflow can miss a notification
trigger. Notifications already placed in the persistent outbox retry delivery.

Migration 005 safely refuses existing nonempty case-variant duplicate emails;
those accounts must be resolved before upgrading. See [Running](running.md).
