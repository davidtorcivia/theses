package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Google's endpoints. They are fields on Drive rather than constants so the
// tests can point the whole flow at an httptest server.
const (
	googleAuth  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleToken = "https://oauth2.googleapis.com/token"
	googleAPI   = "https://www.googleapis.com/drive/v3"
)

// DriveScope is read only, because this integration reads: it lists a folder
// and copies a file into the bucket, and never writes anything back.
const DriveScope = "https://www.googleapis.com/auth/drive.readonly"

// A Token is what the code exchange returned, as it is stored: sealed JSON in
// one settings row. Expires is unix seconds.
type Token struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_at"`
}

// A DriveFile is one entry of a listing. Size is a string in Drive's JSON and
// is absent on a folder.
type DriveFile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Mime   string `json:"mime_type"`
	Size   int64  `json:"size"`
	Folder bool   `json:"folder"`
}

// Drive is Google Drive, files in. The owner pastes an OAuth client id and
// secret, connects with the code flow, and the refresh token that comes back is
// what every later call is made on.
type Drive struct {
	HTTP *http.Client
	// Now is the clock the token expiry is read against, replaced in tests.
	Now func() time.Time
	// Auth, Token and API are Google's endpoints, replaced in tests.
	Auth, TokenURL, API string
	// Save persists a token this package refreshed. The caller seals it and
	// writes the settings row. A failure fails the call that triggered the
	// refresh: the access token in hand would still work, but going on with a
	// refresh token nobody stored means doing the same exchange again on every
	// request from here on, and saying so once is better than that.
	Save func(ctx context.Context, t Token) error

	// mu covers the token, which a refresh replaces. Refreshes are serialised
	// rather than deduplicated: one process, and two requests arriving at the
	// same expired token do the exchange one after the other, which Google
	// allows on the same refresh token.
	//
	// ponytail: the second exchange is wasted work. The upgrade is a
	// singleflight around the refresh, which is worth it only if imports are
	// ever started in bulk.
	mu                     sync.Mutex
	clientID, clientSecret string
	token                  Token
}

func (d *Drive) Name() string { return "Google Drive" }

// Configure reads what the settings table holds. A token that will not parse is
// the one error: it means the row was written under a different secret key or
// by hand, and saying so is better than behaving as though nothing were
// connected and losing the refresh token on the next save.
func (d *Drive) Configure(s Settings) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clientID, d.clientSecret = s["client_id"], s["client_secret"]
	d.token = Token{}
	if raw := s["token"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &d.token); err != nil {
			// The row is there and will not parse, which is the same dead end
			// as a refresh token Google has stopped honouring and has the same
			// answer: connect it again. Why it will not parse is for whoever
			// reads the log, not for the person being told to reconnect.
			slog.Warn("the stored Drive token cannot be read", "err", err)
			return ErrReconnect
		}
	}
	if d.HTTP == nil {
		d.HTTP = Client()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Auth == "" {
		d.Auth, d.TokenURL, d.API = googleAuth, googleToken, googleAPI
	}
	return nil
}

// Configured reports whether the client id and secret are in place, which is
// what the Connect button needs; Connected is whether the flow was finished.
func (d *Drive) Configured() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clientID != "" && d.clientSecret != ""
}

func (d *Drive) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clientID != "" && d.clientSecret != "" && d.token.Refresh != ""
}

// AuthURL is where the owner is sent to consent. access_type=offline and
// prompt=consent are what make Google return a refresh token: without them a
// second connection from the same account comes back with an access token that
// dies in an hour and nothing to renew it with.
func (d *Drive) AuthURL(state, redirect string) string {
	d.mu.Lock()
	id := d.clientID
	auth := d.Auth
	d.mu.Unlock()
	q := url.Values{
		"client_id":     {id},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {DriveScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return auth + "?" + q.Encode()
}

// Exchange trades the code the callback carried for a token and keeps it. The
// caller seals and stores what comes back.
func (d *Drive) Exchange(ctx context.Context, code, redirect string) (Token, error) {
	d.mu.Lock()
	id, secret, endpoint := d.clientID, d.clientSecret, d.TokenURL
	d.mu.Unlock()
	if id == "" || secret == "" {
		return Token{}, ErrNotConnected
	}
	t, err := d.exchange(ctx, endpoint, url.Values{
		"code":          {code},
		"client_id":     {id},
		"client_secret": {secret},
		"redirect_uri":  {redirect},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return Token{}, err
	}
	if t.Refresh == "" {
		return Token{}, fmt.Errorf("Google did not return a refresh token; remove this app from the account's third party access and connect again")
	}
	d.mu.Lock()
	d.token = t
	d.mu.Unlock()
	return t, nil
}

// access returns a usable access token, refreshing first when the stored one is
// within a minute of running out. The minute is slack for the call itself: a
// token that expires while the request is in flight comes back as a 401 nobody
// can do anything with.
func (d *Drive) access(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clientID == "" || d.clientSecret == "" || d.token.Refresh == "" {
		return "", ErrNotConnected
	}
	if d.token.Access != "" && d.token.Expires > d.Now().Add(time.Minute).Unix() {
		return d.token.Access, nil
	}
	t, err := d.exchange(ctx, d.TokenURL, url.Values{
		"client_id":     {d.clientID},
		"client_secret": {d.clientSecret},
		"refresh_token": {d.token.Refresh},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return "", err
	}
	// A refresh answer usually leaves the refresh token out, and the one in
	// hand goes on working. Dropping it here would disconnect Drive on the
	// first renewal.
	if t.Refresh == "" {
		t.Refresh = d.token.Refresh
	}
	d.token = t
	if d.Save != nil {
		if err := d.Save(ctx, t); err != nil {
			return "", fmt.Errorf("the refreshed Drive token could not be stored: %w", err)
		}
	}
	return t.Access, nil
}

func (d *Drive) exchange(ctx context.Context, endpoint string, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return Token{}, provider("Google could not be reached: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		// invalid_grant is the one failure a retry cannot fix: the refresh
		// token was revoked, or the consent was withdrawn.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if strings.Contains(string(body), "invalid_grant") {
			return Token{}, ErrReconnect
		}
		if said := reason(body); said != "" {
			return Token{}, provider("Google said %s: %s", resp.Status, said)
		}
		return Token{}, provider("Google said %s", resp.Status)
	}
	var answer struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		In      int64  `json:"expires_in"`
	}
	if err := decode(resp, &answer); err != nil {
		return Token{}, provider("Google's answer could not be read: %v", err)
	}
	if answer.Access == "" {
		return Token{}, provider("Google returned no access token")
	}
	return Token{
		Access:  answer.Access,
		Refresh: answer.Refresh,
		Expires: d.Now().Add(time.Duration(answer.In) * time.Second).Unix(),
	}, nil
}

// Test is the button on the settings row: the root folder, listed.
func (d *Drive) Test(ctx context.Context) (string, error) {
	rows, err := d.List(ctx, "", "")
	if err != nil {
		return "", err
	}
	folders := 0
	for _, f := range rows {
		if f.Folder {
			folders++
		}
	}
	return fmt.Sprintf("The root folder has %d items, %d of them folders.", len(rows), folders), nil
}

// listPage is how many entries one listing asks for. The pane shows a folder or
// a search, not a file manager, so there is one page and no cursor.
//
// ponytail: a folder with more than this many files shows the first hundred.
// The upgrade is nextPageToken threaded through the handler to the pane.
const listPage = 100

// List returns the entries of one folder, or the matches of a search across the
// account when query is not empty. An empty folder is the root.
func (d *Drive) List(ctx context.Context, folder, query string) ([]DriveFile, error) {
	token, err := d.access(ctx)
	if err != nil {
		return nil, err
	}
	// Drive's q is a language of its own, so the folder id and the search term
	// are quoted into it rather than pasted: a name with a quote in it would
	// otherwise be a clause.
	var clauses []string
	if query = strings.TrimSpace(query); query != "" {
		clauses = append(clauses, "name contains "+quote(query))
	} else {
		if folder == "" {
			folder = "root"
		}
		clauses = append(clauses, quote(folder)+" in parents")
	}
	clauses = append(clauses, "trashed = false")

	q := url.Values{
		"q":                         {strings.Join(clauses, " and ")},
		"fields":                    {"files(id,name,mimeType,size)"},
		"pageSize":                  {strconv.Itoa(listPage)},
		"orderBy":                   {"folder,name"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	var answer struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Mime string `json:"mimeType"`
			Size string `json:"size"`
		} `json:"files"`
	}
	if err := d.call(ctx, token, d.API+"/files?"+q.Encode(), &answer); err != nil {
		return nil, err
	}
	rows := make([]DriveFile, 0, len(answer.Files))
	for _, f := range answer.Files {
		row := DriveFile{ID: f.ID, Name: f.Name, Mime: f.Mime, Folder: f.Mime == folderMime}
		row.Size, _ = strconv.ParseInt(f.Size, 10, 64)
		// A native Docs, Sheets or Slides file has no bytes to copy: it has to
		// be exported to a format first, and there is no one right format to
		// pick on its behalf. It is left out of the listing rather than shown
		// as something that then fails to import.
		if !row.Folder && (row.Size == 0 || strings.HasPrefix(f.Mime, nativePrefix)) {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

const (
	folderMime   = "application/vnd.google-apps.folder"
	nativePrefix = "application/vnd.google-apps."
)

// quote puts a value into Drive's query language as a string literal.
func quote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// Stat reads one file's name, type and size, which is what an import needs
// before it opens anything: the row is created with the size the bucket will
// be asked to hold.
func (d *Drive) Stat(ctx context.Context, id string) (DriveFile, error) {
	token, err := d.access(ctx)
	if err != nil {
		return DriveFile{}, err
	}
	q := url.Values{
		"fields":            {"id,name,mimeType,size"},
		"supportsAllDrives": {"true"},
	}
	var answer struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Mime string `json:"mimeType"`
		Size string `json:"size"`
	}
	if err := d.call(ctx, token, d.API+"/files/"+url.PathEscape(id)+"?"+q.Encode(), &answer); err != nil {
		return DriveFile{}, err
	}
	f := DriveFile{ID: answer.ID, Name: answer.Name, Mime: answer.Mime, Folder: answer.Mime == folderMime}
	f.Size, _ = strconv.ParseInt(answer.Size, 10, 64)
	// All four are facts about what Drive holds rather than faults here, so
	// they carry ErrProvider and a caller answers them as refusals.
	switch {
	case f.Folder:
		return DriveFile{}, provider("that is a folder, not a file")
	case strings.HasPrefix(f.Mime, nativePrefix):
		return DriveFile{}, provider("a Google Docs, Sheets or Slides file has no file to copy; export it to the format you want first")
	case f.Size <= 0:
		return DriveFile{}, provider("that file is empty")
	case f.Size > maxImport:
		return DriveFile{}, provider("that file is larger than the %d GB this can import in one piece", int64(maxImport)>>30)
	}
	return f, nil
}

// Open starts the download of one file. The URL is built here from the id, not
// taken from anything the listing returned, so a hostile answer has no address
// to send this at. The caller closes the body.
func (d *Drive) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	token, err := d.access(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"alt": {"media"}, "supportsAllDrives": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.API+"/files/"+url.PathEscape(id)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, provider("Drive could not be reached: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, failure("Drive", resp)
	}
	return resp.Body, nil
}

func (d *Drive) call(ctx context.Context, token, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return provider("Drive could not be reached: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return failure("Drive", resp)
	}
	if err := decode(resp, into); err != nil {
		return provider("Drive's answer could not be read: %v", err)
	}
	return nil
}
