package settings

// Kind is how a setting's value is parsed from a form and stored as JSON.
type Kind int

const (
	KindString Kind = iota
	KindInt
	KindList   // []string, one entry per repeated form value
	KindChoice // string restricted to Choices
	KindText   // multi-line string
)

// Def describes one known setting. Label and Hint are what /settings prints.
// Min and Max bound a KindInt; a Max of zero means the key is unbounded.
type Def struct {
	Key      string
	Kind     Kind
	Default  any
	Secret   bool
	Internal bool
	Label    string
	Hint     string
	Choices  []string
	Min, Max int
}

// MaxSessionDays is the longest a sign-in may last. Past a year the expiry
// stops being a session and starts being a liability, and a large enough value
// overflows the duration that builds the cookie.
const MaxSessionDays = 365

// defaultEvents is what a new account's email is subscribed to before the
// owner says otherwise: the things that are about them, and the two dates.
const defaultEvents = "assigned\nmentioned\ndue\nstatus\nrelease"

var days = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
var providers = []string{"backblaze", "r2", "s3"}

// Registry is every setting the app knows. A key not here cannot be written.
var Registry = []Def{
	{Key: "workspace.legal_rights_holder", Kind: KindString, Default: "", Label: "Release rights holder"},
	{Key: "workspace.legal_email_subject", Kind: KindString, Default: "{title} is now available", Label: "Participant email subject"},
	{Key: "workspace.legal_email_body", Kind: KindText, Default: "Hello {name},\n\nThank you for taking part in {brand}. The episode is now available:\n\n{url}\n\nThank you,\n{brand}", Label: "Participant email template"},
	{Key: "workspace.name", Kind: KindString, Default: "Workspace", Label: "Name"},
	{Key: "workspace.episode_start", Kind: KindInt, Default: 1, Min: 0, Max: 10000, Label: "Episode numbering starts at"},
	{Key: "workspace.release_day", Kind: KindChoice, Default: "Monday", Choices: days, Label: "Release day"},
	{Key: "workspace.release_time", Kind: KindString, Default: "06:00", Label: "Release time", Hint: "24 hour, in the workspace time zone."},
	{Key: "workspace.timezone", Kind: KindString, Default: "America/New_York", Label: "Time zone", Hint: "An IANA name, for example America/New_York."},

	{Key: "defaults.columns", Kind: KindList, Default: []string{"Research", "Outline", "Script", "Record", "Edit", "Publication"}, Label: "Board columns", Hint: "What a new proposition starts with. Existing boards keep their own."},
	{Key: "defaults.statuses", Kind: KindList, Default: []string{"idea", "researching", "recording", "editing", "released"}, Label: "Statuses", Hint: "In the order a proposition moves through them."},
	{Key: "defaults.question_labels", Kind: KindList, Default: []string{"Is it true?", "Who pays?", "What breaks?", "What do we want?"}, Label: "Question labels", Hint: "The four questions cards are tagged with, in order."},
	{Key: "defaults.document_template", Kind: KindText, Default: "# {statement}\n\n## I. Is it true?\n## II. Who pays?\n## III. What breaks?\n## IV. What do we want?\n## Five consequential facts\n## Historical analogy\n## Street question\n## Expert brief", Label: "Document template", Hint: "Headings every new proposition starts with."},

	{Key: "storage.primary.provider", Kind: KindChoice, Default: "backblaze", Choices: providers, Label: "Provider"},
	{Key: "storage.primary.endpoint", Kind: KindString, Default: "", Label: "Endpoint", Hint: "B2 is https://s3.<region>.backblazeb2.com, or just the host; R2 is https://<account>.r2.cloudflarestorage.com."},
	{Key: "storage.primary.region", Kind: KindString, Default: "", Label: "Region"},
	{Key: "storage.primary.bucket", Kind: KindString, Default: "", Label: "Bucket"},
	{Key: "storage.primary.access_key", Kind: KindString, Default: "", Secret: true, Label: "Access key"},
	{Key: "storage.primary.secret_key", Kind: KindString, Default: "", Secret: true, Label: "Secret key"},
	{Key: "storage.primary.public_base_url", Kind: KindString, Default: "", Label: "Public base URL", Hint: "Optional, for a CDN in front of the bucket."},

	{Key: "storage.recordings.provider", Kind: KindChoice, Default: "backblaze", Choices: providers, Label: "Provider"},
	{Key: "storage.recordings.endpoint", Kind: KindString, Default: "", Label: "Endpoint"},
	{Key: "storage.recordings.region", Kind: KindString, Default: "", Label: "Region"},
	{Key: "storage.recordings.bucket", Kind: KindString, Default: "", Label: "Bucket", Hint: "Leave empty to keep recordings in the primary bucket."},
	{Key: "storage.recordings.access_key", Kind: KindString, Default: "", Secret: true, Label: "Access key"},
	{Key: "storage.recordings.secret_key", Kind: KindString, Default: "", Secret: true, Label: "Secret key"},
	{Key: "storage.recordings.public_base_url", Kind: KindString, Default: "", Label: "Public base URL"},

	{Key: "backups.enabled", Kind: KindChoice, Default: "off", Choices: []string{"off", "on"}, Label: "Nightly backup"},
	{Key: "backups.bucket", Kind: KindString, Default: "", Label: "Bucket", Hint: "Leave empty to write into the primary bucket under the backups/ prefix."},
	{Key: "backups.access_key", Kind: KindString, Default: "", Secret: true, Label: "Access key"},
	{Key: "backups.secret_key", Kind: KindString, Default: "", Secret: true, Label: "Secret key"},
	{Key: "backups.time", Kind: KindString, Default: "03:30", Label: "Time of day", Hint: "24 hour, in the workspace time zone."},
	{Key: "backups.keep", Kind: KindInt, Default: 30, Min: 1, Max: 3650, Label: "How many to keep", Hint: "Days of history the bucket's lifecycle rule and its Object Lock retention are set to. Nothing here deletes a backup."},

	// What the last run did, written by the scheduler, read by the settings page
	// and by readyz. Settings rather than a table because there is one of each
	// and the page already reads settings.
	{Key: "backups.last_at", Kind: KindInt, Default: 0, Internal: true, Label: "Last run"},
	{Key: "backups.last_ok_at", Kind: KindInt, Default: 0, Internal: true, Label: "Last successful run"},
	{Key: "backups.last_size", Kind: KindInt, Default: 0, Internal: true, Label: "Last archive size"},
	{Key: "backups.last_verify", Kind: KindText, Default: "", Internal: true, Label: "Last verification"},
	{Key: "backups.last_error", Kind: KindText, Default: "", Internal: true, Label: "Last failure"},

	{Key: "mail.host", Kind: KindString, Default: "", Label: "SMTP host"},
	{Key: "mail.port", Kind: KindInt, Default: 587, Min: 1, Max: 65535, Label: "Port"},
	{Key: "mail.tls", Kind: KindChoice, Default: "starttls", Choices: []string{"starttls", "tls", "none"}, Label: "TLS"},
	{Key: "mail.user", Kind: KindString, Default: "", Label: "Username"},
	{Key: "mail.password", Kind: KindString, Default: "", Secret: true, Label: "Password"},
	{Key: "mail.from", Kind: KindString, Default: "", Label: "From address"},

	{Key: "signin.require_totp", Kind: KindChoice, Default: "all", Choices: []string{"all", "owners"}, Label: "Require an authenticator", Hint: "Owners are always required to enroll."},
	{Key: "signin.session_days", Kind: KindInt, Default: 30, Min: 1, Max: MaxSessionDays, Label: "Session length", Hint: "Days a sign-in lasts before it has to be repeated, 1 to 365."},
	{Key: "signin.handle_min_length", Kind: KindInt, Default: 2, Min: 2, Max: 32, Label: "Shortest account name", Hint: "Account names are lowercase letters, digits and hyphens, up to 32 characters."},

	{Key: "notify.pushover_token", Kind: KindString, Default: "", Secret: true, Label: "Pushover application token", Hint: "The workspace's own application, so that members paste only their user key."},
	{Key: "notify.ntfy_server", Kind: KindString, Default: "https://ntfy.sh", Label: "ntfy server", Hint: "Where a topic lives when an account names no server of its own."},
	{Key: "notify.digest_time", Kind: KindString, Default: "08:00", Label: "Digest time", Hint: "24 hour, in the workspace time zone. The daily digest and the due date pass both run then."},
	{Key: "notify.defaults", Kind: KindText, Default: defaultEvents, Label: "New accounts are notified about", Hint: "What a new account's email starts subscribed to. Everybody can change their own."},

	// What the daily pass last did, written by the worker and read by it, as a
	// date in the form 20260918. Settings rather than a table because there is
	// one of it, which is what backups.last_at already does.
	{Key: "notify.last_tick", Kind: KindInt, Default: 0, Internal: true, Label: "Last daily pass"},

	// Integrations. Everything an outside service authenticates with is a
	// secret, including the Drive token, which is one JSON object holding the
	// access token, the refresh token and the moment the first runs out.
	{Key: "integrations.drive.client_id", Kind: KindString, Default: "", Secret: true, Label: "Client id", Hint: "From an OAuth client of type Web application in a Google Cloud project with the Drive API enabled."},
	{Key: "integrations.drive.client_secret", Kind: KindString, Default: "", Secret: true, Label: "Client secret"},
	{Key: "integrations.drive.token", Kind: KindString, Default: "", Secret: true, Label: "Drive token"},

	{Key: "integrations.transistor.api_key", Kind: KindString, Default: "", Secret: true, Label: "API key"},
	{Key: "integrations.transistor.show_id", Kind: KindString, Default: "", Label: "Show", Hint: "The show's id. Test with the field empty to be told what the key reaches."},
	{Key: "integrations.transistor.publish_status", Kind: KindString, Default: "released", Label: "Publish at status", Hint: "A proposition can be published once it has reached this status."},
}

var byKey = func() map[string]Def {
	m := make(map[string]Def, len(Registry))
	for _, d := range Registry {
		m[d.Key] = d
	}
	return m
}()

// Lookup returns the definition of key.
func Lookup(key string) (Def, bool) {
	d, ok := byKey[key]
	return d, ok
}
