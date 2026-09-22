# Recording releases

Editors and owners can open **Releases** from a Show or episode page. The create form assigns a link to that workspace and offers street-recording, interview and custom release wording. Choose New York or Georgia as the governing state, enter the rights-holding legal entity, and edit the branding, recording details and agreement. Templates are drafts for legal review, not a guarantee of enforceability. The state selector adds a governing-law clause; recording location belongs in the details.

Set the default legal entity and reusable participant email subject/body in **Settings → Workspace**. Deployment-specific entities and branding belong in settings rather than source code. The release body supports `{rights_holder}`; this is resolved in the public agreement and frozen in each signed copy. Each link also stores its own editable email template.

The public link is `/legal/<token>`, with no participant account required. A form starts with one person; **Add person** expands it, up to 20 people per submission. Each person provides a typed full name, date and explicit confirmation that they are 18 or older and agree to the release and electronic signature. Each person can optionally provide an email and independently opt in to an episode notification. A reusable link accepts any number of submissions.

After signing, a private receipt link displays the exact saved agreement, governing-law clause, consent wording, participants, entered dates and server timestamp. Participants can print/save a PDF or download the JSON record. Everyone sharing a device and submission shares that receipt; use separate submissions when private copies are needed. Receipt links are bearer credentials. Keep them private. The release does not list other participants' submissions.

`/legal/<token>/qr` displays a branded QR card; `/legal/<token>/qr.svg?download=1` downloads the self-contained vector artwork for printing at any size. The code retains its four-module quiet zone and high error correction. URLs use the configured public base URL.

Staff can view/export signed records, update a link for future signers, or close it. Changing wording or state never rewrites a signed agreement. A digest of the complete displayed agreement binds consent even if restoring a backup reuses a version number. An outdated open form must reload and obtain consent again. Resubmitting an identical request after an uncertain connection returns its original receipt. Signed releases cannot move to a different workspace; this prevents expanding access to participants' data. Episodes with releases must be archived rather than deleted so their records remain accessible. Archived episodes accept no new signatures.

**Notify participants** takes an episode URL, subject and body. Use `{name}`, `{brand}`, `{title}` and `{url}`. Preview shows the exact recipients and rendered messages; the send button queues them in the existing SMTP outbox. If recipients or content change after preview, preview again. Nothing sends automatically on publication. Each opted-in email address receives at most one queued notification per release; retries use the existing outbox. Delivery status and failures are available in Mail settings. Saving an email template does not send it. Copies of queued subject/body are retained with the notification record.

Signed records and email addresses require both an editing role and access to the assigned workspace. Their contents do not enter the shared activity feed. Public submissions have CSRF protection, a bounded body, per-address rate limiting, and per-person server-side validation. Agreements and submissions are in SQLite and covered by normal database backups. Restoring an older backup returns records to that backup's point in time.

## API and MCP

All staff operations use the same service as the UI and recheck role and membership. Public signatures are collected only through the consent form; there is no agent tool for signing on behalf of a participant.

| REST route | MCP tool |
| --- | --- |
| `GET /api/v1/legal/releases?proposition=<id>` | `list_releases` |
| `GET /api/v1/legal/releases/<id>` | `get_release` |
| `POST /api/v1/legal/releases` | `save_release` |
| `GET /api/v1/legal/releases/<id>/submissions` | `list_release_submissions` |
| `POST /api/v1/legal/releases/<id>/preview` | `preview_release_email` |
| `POST /api/v1/legal/releases/<id>/notify` | `notify_release_participants` |

Create/update input is a release object: `id` (zero creates), `proposition_id`, `version` (required on updates), `state` (`NY` or `GA`), `kind` (`street`, `interview`, `custom`), `title`, `brand`, `rights_holder`, `details`, `body`, `closed`, `email_subject`, `email_body`. An empty body selects the draft for street/interview. Custom requires a body. Save returns an event with the ID; get the release for its token and current version. The MCP save tool wraps the object as `release`.

Preview/notify take `url`, `subject`, `body`; notify also needs the current `version`. MCP inputs additionally name `id`. Notification calls send email and require explicit user authorization. API clients may invoke notify directly after reviewing their own intended recipients. All reads require read scope; writes and preview require write scope. Use `Idempotency-Key` in REST and `key` in MCP for uncertain-response retries. The browser equivalents are under `/app/legal/releases` with session authentication and CSRF.

## Drafting references

Electronic-signature rules address intent and the connection between a signature and its record; they do not guarantee that every clause of a particular release is enforceable. See [federal electronic-signature law](https://uscode.house.gov/view.xhtml?req=%28title%3A15+section%3A7001+edition%3Aprelim%29), [New York State Technology Law §304](https://www.nysenate.gov/legislation/laws/STT/304), and [Georgia Attorney General guidance](https://consumer.georgia.gov/electronic-signatures). New York's written-consent provisions for use of a person's name, picture and voice are in [Civil Rights Law §51](https://www.nysenate.gov/legislation/laws/CVR/51). The included drafts are original editable starting points; have counsel review the terms for the actual recording and use.
