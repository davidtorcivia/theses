# Workspace workflows

## Show and propositions

Show is the shared workspace for the overall board, notes and file repository.
Its notes sit beneath the board, in the same place as proposition documents.
Use **+ Proposition** to add a linked card: its label follows the proposition's
title and status, while moving the card tracks Show work independently.

Type `@` in supported note or card fields to select a person or proposition.
Arrow keys move the selection; Enter or Tab completes it. Proposition references
use `@[p:ID]` internally and display only information the current account may
read. Copy-link actions address individual cards, documents, blocks, files,
links and comments. Referenced-here results show accessible source passages.

Board controls include saved filters, board/list views and explicit move
controls. Document outlines jump to headings. **Activity** toggles its panel,
with filters, older history, safe undo and **Recently deleted** recovery.

## My work and production

**My work** groups your unfinished assigned cards by due date across accessible
active propositions. The production overview brings together episode status,
owner, next action, blockers and recording/editing/release dates. Production
checklists are ordinary cards, so assignments and completion use the board's
existing controls. Readiness information supports manual decisions; it does
not automatically advance production status or publish an episode.

The [production calendar](calendar.md) also supports direct event/task creation,
U.S. federal and common holidays, and read-only Google/iCloud subscriptions.

## Research and references

Open **Research & references** on a proposition. Evidence can record a source
title, author, year, URL, exact quotation, page/section/timestamp, interpretation
and supported claim. Link evidence to a source URL, file or script block. Mark
it verified after checking the source; editing its fields clears that check
until explicitly reconfirmed. **Copy reference** produces a link to the saved
evidence. Export Markdown for notes or RIS for a reference manager.

## Recording and reviews

A document's **Recording view** pins the last server-saved script with optional
recording cues. Finish syncing edits before pinning. Pinned text stays fixed
while the live document changes. The view includes an outline, reading-size
control, word-count duration estimate and an elapsed timer; it does not record
or edit audio.

Request a review of a document or ready recording and select an eligible
reviewer. Approvals and change requests apply to the requested version. Later
script edits or replacement audio make the old review superseded; request a
new review for the new version. Review history retains the pinned script.

Ready recordings support timestamped comments, comment resolution and
transcripts with editable speaker labels. Import TXT, SRT or VTT; export TXT
or VTT. Optional [local transcription](transcription.md) runs explicitly queued
jobs without a paid hosted transcription API. Separate stereo channels support
speaker estimates; mixed-audio identity detection is not provided.

## Publishing and recovery

For Pinecast, prepare the final audio, show notes and transcript exports here,
then upload and schedule them in the Pinecast dashboard. There is no automatic
Pinecast publishing or synchronization. An optional legacy Transistor connector
remains available for installations that configure it.

Unsaved source drafts are kept on the current device until a confirmed save or
explicit discard; signing out clears local data. **Recently deleted** restores
supported individual items for seven days. It cannot restore a deleted
proposition or recover file bytes removed from storage. See
[recovery](agent-recovery.md) for retries, conflicts and exclusions, and
[connections](connections.md) for agents operating with your permissions.
