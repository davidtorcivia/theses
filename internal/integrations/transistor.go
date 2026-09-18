package integrations

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const transistorAPI = "https://api.transistor.fm"

// An Episode is what a proposition is published as. ID empty means it has not
// been published before; anything else is the episode already on Transistor,
// which is what makes a second publish an update rather than a duplicate.
type Episode struct {
	ID          string
	Title       string
	Summary     string
	Description string
	AudioURL    string
	Number      string
	Season      string
	// Status and ShareURL come back from Transistor rather than going to it.
	Status   string
	ShareURL string
}

// Transistor is the publish-out integration: the owner pastes an API key and
// names the show, and a proposition becomes an episode on it.
type Transistor struct {
	HTTP *http.Client
	// API is the endpoint, replaced in tests.
	API string

	mu   sync.Mutex
	key  string
	show string
}

func (t *Transistor) Name() string { return "Transistor" }

func (t *Transistor) Configure(s Settings) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.key, t.show = s["api_key"], strings.TrimSpace(s["show_id"])
	if t.HTTP == nil {
		t.HTTP = Client()
	}
	if t.API == "" {
		t.API = transistorAPI
	}
	return nil
}

func (t *Transistor) Connected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.key != "" && t.show != ""
}

// Test is the button on the settings row: the show, fetched. With no show id
// saved it lists what the key can see, so the owner can read the id off the
// page rather than out of a Transistor URL.
func (t *Transistor) Test(ctx context.Context) (string, error) {
	t.mu.Lock()
	key, show, api := t.key, t.show, t.API
	t.mu.Unlock()
	if key == "" {
		return "", ErrNotConnected
	}
	if show == "" {
		var answer struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Title string `json:"title"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if err := t.call(ctx, http.MethodGet, api+"/v1/shows", nil, &answer); err != nil {
			return "", err
		}
		if len(answer.Data) == 0 {
			return "", fmt.Errorf("that key reaches no shows")
		}
		said := make([]string, 0, len(answer.Data))
		for _, s := range answer.Data {
			said = append(said, s.Attributes.Title+" ("+s.ID+")")
		}
		return "That key reaches " + strings.Join(said, ", ") + ". Put the id in the show field.", nil
	}
	var answer struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Title string `json:"title"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := t.call(ctx, http.MethodGet, api+"/v1/shows/"+url.PathEscape(show), nil, &answer); err != nil {
		return "", err
	}
	return "Connected to " + answer.Data.Attributes.Title + ".", nil
}

// Publish creates or updates the episode and moves it to published. It is
// written to be run twice: with an id it updates that episode in place, and it
// asks Transistor to publish only an episode that is not published already, so
// a second run does not move the release date.
func (t *Transistor) Publish(ctx context.Context, e Episode) (Episode, error) {
	t.mu.Lock()
	key, show, api := t.key, t.show, t.API
	t.mu.Unlock()
	if key == "" || show == "" {
		return Episode{}, ErrNotConnected
	}

	form := url.Values{
		"episode[title]":       {e.Title},
		"episode[summary]":     {e.Summary},
		"episode[description]": {e.Description},
		"episode[audio_url]":   {e.AudioURL},
	}
	if e.Number != "" {
		form.Set("episode[number]", e.Number)
	}
	if e.Season != "" {
		form.Set("episode[season]", e.Season)
	}

	method, where := http.MethodPost, api+"/v1/episodes"
	if e.ID == "" {
		form.Set("episode[show_id]", show)
	} else {
		method, where = http.MethodPatch, api+"/v1/episodes/"+url.PathEscape(e.ID)
	}
	out, err := t.episode(ctx, method, where, form)
	if err != nil {
		return Episode{}, err
	}
	if out.ID == "" {
		return Episode{}, fmt.Errorf("Transistor returned no episode id")
	}
	if out.Status == "published" {
		return out, nil
	}
	// Publishing is its own endpoint. Everything above is the draft, so an
	// episode that fails here is still on Transistor with the right contents
	// and the same id, and running this again finishes it.
	published, err := t.episode(ctx, http.MethodPatch,
		api+"/v1/episodes/"+url.PathEscape(out.ID)+"/publish",
		url.Values{"episode[status]": {"published"}})
	if err != nil {
		// The id is returned beside the error so the caller can record it: an
		// episode that exists and was not recorded is the one thing that would
		// make the next publish a duplicate.
		return out, err
	}
	published.ID = out.ID
	if published.ShareURL == "" {
		published.ShareURL = out.ShareURL
	}
	return published, nil
}

func (t *Transistor) episode(ctx context.Context, method, where string, form url.Values) (Episode, error) {
	var answer struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Title    string `json:"title"`
				Status   string `json:"status"`
				ShareURL string `json:"share_url"`
				Number   any    `json:"number"`
				Season   any    `json:"season"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := t.call(ctx, method, where, form, &answer); err != nil {
		return Episode{}, err
	}
	return Episode{
		ID:       answer.Data.ID,
		Title:    answer.Data.Attributes.Title,
		Status:   answer.Data.Attributes.Status,
		ShareURL: answer.Data.Attributes.ShareURL,
		Number:   text(answer.Data.Attributes.Number),
		Season:   text(answer.Data.Attributes.Season),
	}, nil
}

// text is a JSON number or string as a string. Transistor sends an episode
// number as a number and takes it back as a form field.
func text(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatInt(int64(n), 10)
	}
	return ""
}

func (t *Transistor) call(ctx context.Context, method, where string, form url.Values, into any) error {
	t.mu.Lock()
	key := t.key
	t.mu.Unlock()

	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, where, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Transistor could not be reached: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return fmt.Errorf("Transistor refused the API key")
	}
	if resp.StatusCode/100 != 2 {
		return failure("Transistor", resp)
	}
	if err := decode(resp, into); err != nil {
		return fmt.Errorf("Transistor's answer could not be read: %w", err)
	}
	return nil
}
