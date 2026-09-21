# Follow-up application audit, 2026-09-20

Historical release report. Findings and limits below describe this audit at the time, not the current application. Later releases added durable source drafts and notification matching, browser CI and performance fixtures, and deleted-item recovery. See [Running](running.md), [Quality](quality.md) and [Recovery](agent-recovery.md) for current behavior.

This audit covers account security, HTTP/API boundaries, storage configuration,
backup recovery, board commands, document import, notification delivery,
browser editing, offline session handling, keyboard access, and desktop/mobile
layout. It builds on the [earlier audit](audit.md). No runtime dependencies or
database migrations were added.

## Fixed findings

| Area | Defect and fix |
| --- | --- |
| Account recovery | A session alone could replace the recovery email or authenticator. Both changes now require the current password and the enrolled authenticator code. Ordinary profile edits still require only the session. |
| Authenticator enrollment | An audit write could succeed before the new secret failed to save. Secret replacement and its activity row now share one transaction. |
| Storage CSP | Native AWS S3 with an empty endpoint produced URLs that the browser CSP blocked. The SDK resolves the permitted origin using the same region, bucket, and path-style addressing as the presigner. Tests include standard, China, and GovCloud regions. |
| Bootstrap URL | Credentials, paths, queries, and fragments were accepted in a base URL even though generated routes require an origin. Startup now refuses those configurations. |
| Restore | A manifest account count was trusted without checking the extracted database. Restores now verify database integrity, foreign keys, and an owner account before replacing live data. |
| Daily reminders | A transient failure could record the day as finished before any reminders were queued. Candidate reads, the outbox, and the daily marker now share one transaction; failed and concurrent ticks are covered by regression tests. |
| Notification API | Replacing several channels and their rules could save a prefix before returning an error. The complete PUT now commits or rolls back together, including deletion and injected storage failures. |
| Digest email | Every item linked to the first item's proposition. Each digest entry now retains its own URL. |
| Scheduling | Invalid release dates were accepted and then silently skipped by reminders. The shared command requires a real ISO calendar date, and the settings form uses a native date input. |
| Assignment | Unknown assignees were silently omitted during card creation, and outsiders could be assigned cards they could not read. Shared validation now requires an owner or proposition member and rolls back invalid card creation. Pickers show eligible users and retain removed members only so they can be unassigned. |
| Markdown import | A valid first edit could commit before a later block failed validation. The revision and every imported block now share one transaction; command or validation failures retain both the original database content and the hand-edited file. |
| Document settings | Editing, retention, and public-reading-page switches were stored but never enforced. The unused controls and write path were removed. The page now states actual role-based editing, retained history, and private document access. |
| Mention picker | Arrow navigation moved focus out of unfinished edits, Enter/Escape reached the editor's own handlers, and contenteditable selection lost its caret. Suggestions now retain editor focus, consume their own keys, and restore the insertion position across text nodes. Plaintext editing preserves description newlines. |
| Keyboard actions | Rail menus, assignment, completion, and comment-delete buttons could receive focus while invisible. They now become visible on focus within their parent. |
| Upload recovery | A transient failure while still online had no retry action. Failed uploads now offer Retry through the existing resumable upload flow, including failures before a server file ID exists. |
| Notification form | Redirected validation and login pages were reported as successful saves. Autosave now recognizes the actual success redirect and handles session expiry. |
| Sign-out | There was no ordinary sign-out control. Profile now offers sign-out for this browser alongside sign-out everywhere. |
| Revision comparison | The history dialog allocated an unbounded quadratic table. Exact comparisons are capped at 250,000 cells; larger comparisons preserve common edges and show the middle as removals and additions. |
| Session expiry | Event polling and revision history could treat a login redirect as an offline or parsing error. They now clear cached account state and return to sign-in when the response is the application's login page. Other HTML failures retain local work. |
| Mobile navigation | The closed rail remained keyboard-focusable off screen. It is now inert while closed on narrow screens, with expanded-state semantics and Escape focus restoration. Search has dialog semantics, contains Tab navigation, preserves focus when delayed results redraw, and restores focus on close. |

## Verification

Regression checks cover transaction rollback, concurrent reminder ticks,
account reauthentication, storage URL origins, import failure, assignment
permissions, notification redirects, mention keyboard/caret behavior, and
bounded diff reconstruction. The full Go and browser-module suites, formatting,
JavaScript syntax checks, vet, race detection, and a CGO-disabled production
build passed.

Headless Chromium exercises a fresh temporary workspace: owner setup and TOTP
enrollment, proposition and card creation, mention selection, document saving
and reload, profile forms, mobile navigation, and search focus. Screenshots are
inspected at 1440 and 390 pixels; the inspected pages have no horizontal
viewport overflow. Browser interaction does not use production data.

The Go vulnerability scan reports no reachable vulnerabilities and none in
imported packages. One advisory belongs to an unused package in a required
module, as in the earlier audit.

A synthetic comparison of two entirely different 2,000-line documents was run
five times per implementation in separate Node 24.20 processes on the same
machine. Median comparison time was 50.00 ms before and 0.85 ms after; median
process peak RSS was 114.7 MiB before and 46.6 MiB after. Both outputs retained
all 4,000 removed/added lines. The large-comparison fallback deliberately gives
a coarser alignment; these measurements are not application-wide latency or
throughput results.

## Boundaries

Provider integrations are tested with local fakes, not live external accounts.
Headless Chromium does not substitute for physical touch-device or screen-reader
testing. Source drafts and the remaining offline restrictions described in the
earlier audit still apply. Notification matching from the in-process command
bus remains best effort; this change makes daily scheduled reminders durable,
not the entire event subscription. Markdown import commits the database before
rewriting its mirror; a later filesystem failure can therefore leave committed
database changes and an unreplaced mirror.

Public document reading pages, automatic revision retention, and integrations
not implemented by the application are not presented as available features.
The existing two-minute revision interval is retained: its implementation and
UI document a later decision than the original ten-minute plan. Explicit HTTP
base URLs remain supported; HTTPS is needed for Secure cookies.
