# Agent workflows and recovery

Calendar entries, production plans, evidence, pinned scripts, review requests and decisions, transcripts, transcription jobs, audio comment resolution, and deleted-item recovery now have matching REST and MCP operations. The 24 workflow tools use current account permissions, structured output schemas, and optional retry keys. Tests compare the published route/tool table with the registered handlers and tool schemas. [API reference](api.md) and [agent guide](SKILLS.md) document the contract.

Personal and owner-created keys can expire after 7, 30, 90 or 365 days. New forms default to 30 days, with no expiry available. Existing keys retain their lifetime. Expiry is checked on every HTTP request, including initialized MCP sessions. File restoration requires both write and files scopes.

Activity contains a Recently deleted view. Individual cards, documents, links, completed files, evidence and calendar events can be restored for seven days. The snapshot and deletion commit together. Restoration preserves resource IDs, restores owned rows and surviving references, advances edit versions, and refuses conflicts atomically. Deleted accounts retain normal database semantics: nullable authors stay null and obsolete assignees stay removed. Restore and object cleanup share the existing maintenance reservation.

Deleted documents retain unsaved source drafts on the device, expose their text for copying, and offer recovery after the document is restored. Both recovery after reload and recovery within the same session are covered. Restored blocks cannot be overwritten by older events. Attachments, research, recording comments and transcript caches refresh after restoration.

Uncertain script pins, review requests, transcript passage saves, Drive imports and restoration retries keep the same request key and payload. Completed Drive retries no longer depend on the original source, and abandoned copies release their retry mapping. Server-side imports serialize to prevent overlapping retries from changing the same object; browser uploads remain independent. Online link creation uses the existing durable queue. File and attachment mutation responses carry their event sequence; late resource reads cannot replace newer file events or resurrect deleted rows. Uploads retain their original destination during navigation, confirm previously completed uploads before resuming, and distinguish unreadable recovery storage from an empty queue.

Ordinary browser API requests have a 30-second deadline; Drive import allows the server's six-hour copy deadline plus one minute for the response. Bucket uploads stop after two minutes without progress and remain resumable. Status announcements use a live region. Confirmation, attachment, history and Drive dialogs have accessible names. Recovery controls wrap on narrow screens and use 40-pixel button heights.

## Validation

The full Go suite, vet, formatting and JavaScript syntax checks pass. All 17 Node test files pass. Race checks cover realtime, core, notifications, backups, files and workflow packages. Dependency checks report no reachable Go vulnerabilities and no npm vulnerabilities; the existing unused-package module advisory remains outside imported code.

Chromium covers complete workflows, offline recovery, committed requests whose responses are lost, same-session and reload draft recovery, restored content conflicts, browser-storage failure/retry, named dialogs, touch emulation, and 320/390/768/1024-pixel recovery layouts. These checks do not replace physical-device or screen-reader testing.

The expanded click-feedback fixture has 500 cards and 200 long document paragraphs. Slow cases add 300 ms per API request and 2x CPU throttling. Each scenario records 40 interactions, measuring click-handler entry to the second animation frame on the same local rig:

| Scenario | Before p95 | After p95 |
| --- | ---: | ---: |
| Show | 32.5 ms | 32.6 ms |
| Large workspace | 43.0 ms | 47.2 ms |
| Slow Show | 31.4 ms | 31.4 ms |
| Slow large workspace | 66.6 ms | 66.0 ms |

All scenarios meet the 150 ms lab budget. The measurements do not establish a speedup or a network-completion guarantee. Field INP still requires real-user measurements.

## Boundaries

Proposition deletion, individual comment/checklist deletion and incomplete uploads are not recoverable through trash. File recovery requires the original object to remain in storage. Pending transcription jobs are not restarted. Internal revision/checklist IDs may change. Workflow retry attempts outside the durable link queue are held in the open UI; closing it or reloading can lose the retry key. Per-key proposition allowlists are not implemented; narrower access uses the account's memberships.
