// Package integrations wires the workspace to the services outside it. Two are
// built: Google Drive, which files land from, and Transistor, which episodes
// are published to. Riverside and Descript are not built; when they are, they
// are another type in this package implementing the same interface, and a row
// beside the other two on the settings page.
//
// Nothing here follows a URL a provider handed back. A Drive download is built
// from the file id against the Drive endpoint this package holds, never from
// the webContentLink in the listing, and Transistor's share_url is printed as a
// link and never fetched. That, and the fact that every call goes through the
// client in safehttp with its resolved address check, is the whole answer to
// what a hostile provider response can reach.
//
// Secrets never reach an error. The messages built here carry a status and
// whatever the provider called the problem; keys, tokens and signed URLs are
// not put into them, and the settings page redacts what it knows anyway.
package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/davidtorcivia/theses/internal/safehttp"
)

// An Integration is one service the workspace is connected to. Name is the row
// on the settings page, Configure hands it what the settings table holds for
// it, Connected says whether that was enough, and Test is the button beside the
// row. Whatever else an integration does is its own: Drive lists and opens
// files, Transistor publishes an episode, and neither belongs on an interface
// the other has to carry.
type Integration interface {
	Name() string
	Configure(Settings) error
	Connected() bool
	Test(ctx context.Context) (string, error)
}

var (
	_ Integration = (*Drive)(nil)
	_ Integration = (*Transistor)(nil)
)

// Settings is what an integration was configured with: the keys under its own
// prefix in the settings table, with the secrets already unsealed by the
// caller. Nothing in this package reaches the database.
type Settings map[string]string

// ErrNotConnected is what an operation returns before the owner has finished
// connecting the integration in settings.
var ErrNotConnected = errors.New("that integration is not connected yet; a workspace owner does that in settings")

// ErrReconnect is a refresh token the provider will not honour any more. It is
// separated from every other failure because the answer to it is a person
// pressing Connect again, not a retry.
var ErrReconnect = errors.New("the connection to that service has expired; a workspace owner has to connect it again")

// ErrProvider marks anything that went wrong at the other end rather than in
// this server: a status outside 2xx, an answer that will not parse, a service
// that cannot be reached, and a file this cannot copy. None of it means this
// side broke, and all of it is worth reading, so a caller mapping errors to
// statuses answers it as a refusal carrying its message rather than logging it
// and returning a 500. It is never returned on its own; it is what the errors
// below unwrap to.
var ErrProvider = errors.New("that service refused it")

// providerError is one of those messages. It is a type rather than a wrap of
// ErrProvider because a wrap would put "that service refused it: " in front of
// every one of them, and most are a plain sentence that reads better alone.
// errors.Is still matches, because Unwrap says so.
type providerError struct{ msg string }

func (e providerError) Error() string { return e.msg }
func (e providerError) Unwrap() error { return ErrProvider }

// provider tags a message as the other end's.
func provider(format string, args ...any) error {
	return providerError{msg: fmt.Sprintf(format, args...)}
}

// maxImport is the largest object an import may stream. It is the S3 limit on
// a single PutObject, which is what the import writes with.
//
// ponytail: a larger file needs the server side of a multipart upload, which
// blob only has the presigned half of. The ceiling is 5 GiB and the upgrade is
// UploadPart on the blob client and a loop here.
const maxImport = 5 << 30

// maxJSON is how much of an answer this package will parse. The client's own
// cap is the import ceiling, because the same client streams a file; a reply
// that is meant to be a few kilobytes of JSON gets this one instead.
const maxJSON = 1 << 20

// Client is the outbound client the integrations use: safehttp's, so every
// address is resolved and checked before it is dialled, with no whole-request
// timeout because an import streams for as long as the file takes. What bounds
// a call is the context the caller passes and the transport's own header and
// dial timeouts.
func Client() *http.Client {
	c := safehttp.Client(safehttp.MaxBytes(maxImport))
	c.Timeout = 0
	return c
}

// decode reads a JSON body, capped, and closes it.
func decode(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if into == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSON))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(into)
}

// failure turns a response outside 2xx into a message a person can act on. The
// body is read for the provider's own words and cut short; what is not JSON is
// reported as the status alone, because an HTML error page pasted onto a
// settings row says nothing and can be long.
func failure(name string, resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if said := reason(body); said != "" {
		return provider("%s said %s: %s", name, resp.Status, said)
	}
	return provider("%s said %s", name, resp.Status)
}

// reason digs the message out of the two error shapes these two APIs use:
// Google's {"error":{"message":...}} or {"error":"...","error_description":...},
// and Transistor's JSON:API {"errors":[{"title":...}]}.
func reason(body []byte) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
		Desc  string          `json:"error_description"`
		Errs  []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, e := range payload.Errs {
		if said := first(e.Title, e.Detail); said != "" {
			return said
		}
	}
	var object struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload.Error, &object) == nil && object.Message != "" {
		return object.Message
	}
	var plain string
	if json.Unmarshal(payload.Error, &plain) == nil {
		return first(payload.Desc, plain)
	}
	return payload.Desc
}

func first(of ...string) string {
	for _, s := range of {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}
