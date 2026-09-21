package server

import "context"

func (s *Server) diagnostics(ctx context.Context) (map[string]any, error) {
	var unmatched, pending, failed, uploads int64
	err := s.db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM activity WHERE id>(SELECT activity_id FROM notification_cursor WHERE id=1)),
 (SELECT count(*) FROM notification_outbox WHERE sent_at IS NULL),
 (SELECT count(*) FROM notification_outbox WHERE sent_at IS NULL AND tried_at<unixepoch()-86400),
 (SELECT count(*) FROM files f LEFT JOIN uploads u ON u.file_id=f.id WHERE f.state='uploading' AND ((u.id IS NOT NULL AND u.expires_at<unixepoch()) OR (u.id IS NULL AND f.created_at<unixepoch()-172800)))`).Scan(&unmatched, &pending, &failed, &uploads)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"notification_unmatched": unmatched, "notification_pending": pending, "notification_failed": failed, "expired_uploads": uploads,
		"core_refusals_since_restart": s.board.Rejected.Load(), "upload_completion_failures_since_restart": s.files.CompletionFailures.Load(), "mirror_write_failures_since_restart": s.docs.MirrorFailures.Load(),
		"backups_enabled": s.backups.Enabled(), "backups_stale": s.backups.CheckAge(ctx) != nil,
	}, nil
}
