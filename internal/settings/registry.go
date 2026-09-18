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
	Label    string
	Hint     string
	Choices  []string
	Min, Max int
}

// MaxSessionDays is the longest a sign-in may last. Past a year the expiry
// stops being a session and starts being a liability, and a large enough value
// overflows the duration that builds the cookie.
const MaxSessionDays = 365

var days = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
var providers = []string{"backblaze", "r2", "s3"}

// Registry is every setting the app knows. A key not here cannot be written.
var Registry = []Def{
	{Key: "workspace.name", Kind: KindString, Default: "We All Fall Down", Label: "Name"},
	{Key: "workspace.episode_start", Kind: KindInt, Default: 1, Min: 0, Max: 10000, Label: "Episode numbering starts at"},
	{Key: "workspace.release_day", Kind: KindChoice, Default: "Monday", Choices: days, Label: "Release day"},
	{Key: "workspace.release_time", Kind: KindString, Default: "06:00", Label: "Release time", Hint: "24 hour, in the workspace time zone."},
	{Key: "workspace.timezone", Kind: KindString, Default: "America/New_York", Label: "Time zone", Hint: "An IANA name, for example America/New_York."},

	{Key: "defaults.columns", Kind: KindList, Default: []string{"Research", "Outline", "Script", "Record", "Edit", "Publication"}, Label: "Board columns", Hint: "What a new proposition starts with. Existing boards keep their own."},
	{Key: "defaults.statuses", Kind: KindList, Default: []string{"idea", "researching", "recording", "editing", "released"}, Label: "Statuses", Hint: "In the order a proposition moves through them."},
	{Key: "defaults.question_labels", Kind: KindList, Default: []string{"Is it true?", "Who gets fucked?", "What breaks?", "Then what do we want?"}, Label: "Question labels", Hint: "The four questions cards are tagged with, in order."},
	{Key: "defaults.document_template", Kind: KindText, Default: "# {statement}\n\n## I. Is it true?\n## II. Who gets fucked?\n## III. What breaks?\n## IV. Then what do we want?\n## Five consequential facts\n## Historical analogy\n## Street question\n## Expert brief", Label: "Document template", Hint: "Headings every new proposition starts with."},

	{Key: "storage.primary.provider", Kind: KindChoice, Default: "backblaze", Choices: providers, Label: "Provider"},
	{Key: "storage.primary.endpoint", Kind: KindString, Default: "", Label: "Endpoint", Hint: "B2 is s3.<region>.backblazeb2.com; R2 is <account>.r2.cloudflarestorage.com."},
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

	{Key: "mail.host", Kind: KindString, Default: "", Label: "SMTP host"},
	{Key: "mail.port", Kind: KindInt, Default: 587, Min: 1, Max: 65535, Label: "Port"},
	{Key: "mail.tls", Kind: KindChoice, Default: "starttls", Choices: []string{"starttls", "tls", "none"}, Label: "TLS"},
	{Key: "mail.user", Kind: KindString, Default: "", Label: "Username"},
	{Key: "mail.password", Kind: KindString, Default: "", Secret: true, Label: "Password"},
	{Key: "mail.from", Kind: KindString, Default: "", Label: "From address"},

	{Key: "signin.require_totp", Kind: KindChoice, Default: "all", Choices: []string{"all", "owners"}, Label: "Require an authenticator", Hint: "Owners are always required to enrol."},
	{Key: "signin.session_days", Kind: KindInt, Default: 30, Min: 1, Max: MaxSessionDays, Label: "Session length", Hint: "Days a sign-in lasts before it has to be repeated, 1 to 365."},
	{Key: "signin.handle_min_length", Kind: KindInt, Default: 2, Min: 2, Max: 32, Label: "Shortest account name", Hint: "Account names are lowercase letters, digits and hyphens, up to 32 characters."},
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
