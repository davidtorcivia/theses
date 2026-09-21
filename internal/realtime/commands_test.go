package realtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPCommandsShareSocketValidationAndIdempotency(t *testing.T) {
	r := newRig(t)
	call := func(who, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/app/commands", strings.NewReader(body))
		req.Header.Set("Cookie", r.cookie[who])
		w := httptest.NewRecorder()
		r.hub.Commands(w, req)
		return w
	}
	body := fmt.Sprintf(`{"id":1,"cmd":"card.create","key":"http-command","args":{"column":%d,"title":"HTTP card"}}`, r.cols[0].ID)
	var seq int64
	for i := 0; i < 2; i++ {
		w := call("grace", body)
		var m message
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &m) != nil || m.Type != "ack" || m.Event == nil {
			t.Fatalf("command: %d %s", w.Code, w.Body)
		}
		if i == 0 {
			seq = m.Event.Seq
		} else if m.Event.Seq != seq || !m.Event.Replayed {
			t.Fatalf("retry: %+v", m)
		}
	}
	for _, tc := range []struct {
		who, body string
		status    int
		kind      string
	}{
		{"", body, http.StatusUnauthorized, ""},
		{"stranger", body, http.StatusOK, "error"},
		{"grace", "{} {}", http.StatusBadRequest, ""},
		{"grace", strings.Repeat("x", maxFrame+1), http.StatusRequestEntityTooLarge, ""},
		{"grace", `{"cmd":"unknown"}`, http.StatusOK, "error"},
		{"grace", `{"cmd":"card.create","key":"bad key"}`, http.StatusOK, "error"},
	} {
		w := call(tc.who, tc.body)
		if w.Code != tc.status {
			t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
		}
		if tc.kind != "" {
			var m message
			if json.Unmarshal(w.Body.Bytes(), &m) != nil || m.Type != tc.kind {
				t.Fatalf("response: %s", w.Body)
			}
		}
	}
}
