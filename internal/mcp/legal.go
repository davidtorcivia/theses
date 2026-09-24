package mcp

import (
	"context"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/legal"
)

type releaseSave struct {
	workflowKey
	Release legal.Release `json:"release"`
}
type releaseNotify struct {
	workflowKey
	ID          int64  `json:"id"`
	Version     int64  `json:"version"`
	URL         string `json:"url"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	PreviewHash string `json:"preview_hash,omitempty" jsonschema:"preview_hash from preview_release_email; notify refuses if recipients or messages changed since"`
}
type releasePreview struct {
	Messages    []legal.Message `json:"messages"`
	PreviewHash string          `json:"preview_hash"`
}

func Legal(s *Server) {
	l := s.api.Legal
	workflowTool(s, "list_releases", "List recording release links assigned to an accessible show or episode. Requires an editor role.", auth.ScopeRead, reads("List releases"), func(ctx context.Context, a core.Actor, in propositionArgs) ([]legal.Release, error) {
		return l.List(ctx, a, in.Proposition)
	})
	workflowTool(s, "get_release", "Read a release, its public token and current version.", auth.ScopeRead, reads("Get release"), func(ctx context.Context, a core.Actor, in workflowID) (legal.Release, error) {
		return l.Get(ctx, a, in.ID)
	})
	workflowTool(s, "save_release", "Create (id zero) or update a release using its current version. Closed links stop new signatures. Signed wording is immutable. Body may use {rights_holder}. Public path is /legal/{token}; append /qr for a QR card.", auth.ScopeWrite, changes("Save release"), func(ctx context.Context, a core.Actor, in releaseSave) (core.Event, error) {
		return l.Save(ctx, a, in.Release)
	})
	workflowTool(s, "list_release_submissions", "Read or export signed agreements with participant names and optional email addresses. Requires an editor role.", auth.ScopeRead, reads("Read signed releases"), func(ctx context.Context, a core.Actor, in workflowID) ([]legal.Submission, error) {
		return l.Submissions(ctx, a, in.ID)
	})
	workflowTool(s, "preview_release_email", "Preview messages for opted-in participants who have not already been notified, with a preview_hash to pass to notify. Does not send email.", auth.ScopeWrite, reads("Preview participant email"), func(ctx context.Context, a core.Actor, in releaseNotify) (releasePreview, error) {
		messages, err := l.Preview(ctx, a, in.ID, in.URL, in.Subject, in.Body)
		return releasePreview{messages, legal.MessageDigest(messages)}, err
	})
	workflowTool(s, "notify_release_participants", "Explicitly queue an episode notification for opted-in participants. Sends email; get user authorization before calling. Each email address is notified once per release.", auth.ScopeWrite, changes("Notify participants"), func(ctx context.Context, a core.Actor, in releaseNotify) (core.Event, error) {
		var expected []string
		if in.PreviewHash != "" {
			expected = []string{in.PreviewHash}
		}
		return l.Notify(ctx, a, in.ID, in.Version, in.URL, in.Subject, in.Body, expected...)
	})
}
