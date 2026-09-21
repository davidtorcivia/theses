package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNotFound is what every lookup here returns when the row is absent, so
// callers do not have to know about sql.ErrNoRows.
var ErrNotFound = errors.New("not found")

type User struct {
	ID           int64
	Handle       string
	Email        string
	Name         string
	Initials     string
	Colour       string
	Role         string
	PasswordHash string
	TOTPSecret   string
	TOTPLastStep int64
	SessionEpoch int64
	CreatedAt    int64
	LastSeenAt   sql.NullInt64
}

const userColumns = `id, handle, email, name, initials, colour, role, password_hash,
	totp_secret, totp_last_step, session_epoch, created_at, last_seen_at`

func scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.Name, &u.Initials, &u.Colour, &u.Role,
		&u.PasswordHash, &u.TOTPSecret, &u.TOTPLastStep, &u.SessionEpoch, &u.CreatedAt, &u.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func CreateUser(ctx context.Context, q Querier, u *User) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO users
		(handle, email, name, initials, colour, role, password_hash, totp_secret, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, unixepoch())`,
		u.Handle, u.Email, u.Name, u.Initials, u.Colour, u.Role, u.PasswordHash, u.TOTPSecret)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func UserByID(ctx context.Context, q Querier, id int64) (*User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func UserByHandle(ctx context.Context, q Querier, handle string) (*User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE handle = ?`, handle))
}

func UserByEmail(ctx context.Context, q Querier, email string) (*User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email) = lower(?)`, email))
}

// UserByHandleOrEmail backs password reset, where the person may type either.
func UserByHandleOrEmail(ctx context.Context, q Querier, v string) (*User, error) {
	return scanUser(q.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE handle = ? OR lower(email) = lower(?)`, v, v))
}

func ListUsers(ctx context.Context, q Querier) ([]*User, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Handle, &u.Email, &u.Name, &u.Initials, &u.Colour, &u.Role,
			&u.PasswordHash, &u.TOTPSecret, &u.TOTPLastStep, &u.SessionEpoch, &u.CreatedAt, &u.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func CountUsers(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func UpdateProfile(ctx context.Context, q Querier, id int64, handle, name, initials, colour, email string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE users SET handle = ?, name = ?, initials = ?, colour = ?, email = ? WHERE id = ?`,
		handle, name, initials, colour, email, id)
	return err
}

// SetUserRoleKeepingAnOwner changes a role but never leaves the workspace
// without an owner, and reports whether it did. The count and the write are one
// statement, so two owners demoting each other at the same moment cannot both
// pass their own check and leave nobody able to reach the settings.
func SetUserRoleKeepingAnOwner(ctx context.Context, q Querier, id int64, role string) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE users SET role = ?
		WHERE id = ? AND (role <> 'owner'
			OR (SELECT count(*) FROM users WHERE role = 'owner' AND id <> ?) > 0)`, role, id, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteUserKeepingAnOwner is the same guard for deleting an account.
func DeleteUserKeepingAnOwner(ctx context.Context, q Querier, id int64) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM users
		WHERE id = ? AND (role <> 'owner'
			OR (SELECT count(*) FROM users WHERE role = 'owner' AND id <> ?) > 0)`, id, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func SetPasswordHash(ctx context.Context, q Querier, id int64, hash string) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

func SetTOTPSecret(ctx context.Context, q Querier, id int64, secret string) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_last_step = 0 WHERE id = ?`, secret, id)
	return err
}

// ClaimTOTPStep records a time step as used and reports whether it was still
// unused. The comparison and the write are one statement so two requests with
// the same code cannot both succeed.
func ClaimTOTPStep(ctx context.Context, q Querier, id, step int64) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, id, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BumpSessionEpoch is "sign out everywhere": existing sessions carry the old epoch.
func BumpSessionEpoch(ctx context.Context, q Querier, id int64) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET session_epoch = session_epoch + 1 WHERE id = ?`, id)
	return err
}

func TouchUser(ctx context.Context, q Querier, id int64) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET last_seen_at = unixepoch() WHERE id = ?`, id)
	return err
}

type Session struct {
	ID        int64
	UserID    int64
	Epoch     int64
	ExpiresAt int64
	UserAgent string
}

func CreateSession(ctx context.Context, q Querier, userID int64, hmac []byte, epoch, expiresAt int64, userAgent string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO sessions
		(user_id, hmac, epoch, created_at, expires_at, user_agent)
		VALUES (?, ?, ?, unixepoch(), ?, ?)`, userID, hmac, epoch, expiresAt, userAgent)
	return err
}

// SessionByHMAC returns the session and its user only if the session is unexpired
// and still on the user's current epoch.
func SessionByHMAC(ctx context.Context, q Querier, hmac []byte, now int64) (*Session, *User, error) {
	var s Session
	var u User
	err := q.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.epoch, s.expires_at, s.user_agent,
		u.id, u.handle, u.email, u.name, u.initials, u.colour, u.role, u.password_hash,
		u.totp_secret, u.totp_last_step, u.session_epoch, u.created_at, u.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.hmac = ? AND s.expires_at > ? AND s.epoch = u.session_epoch`, hmac, now).
		Scan(&s.ID, &s.UserID, &s.Epoch, &s.ExpiresAt, &s.UserAgent,
			&u.ID, &u.Handle, &u.Email, &u.Name, &u.Initials, &u.Colour, &u.Role, &u.PasswordHash,
			&u.TOTPSecret, &u.TOTPLastStep, &u.SessionEpoch, &u.CreatedAt, &u.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return &s, &u, nil
}

func DeleteSession(ctx context.Context, q Querier, hmac []byte) error {
	_, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE hmac = ?`, hmac)
	return err
}

type Invitation struct {
	ID              int64
	Email           string
	Role            string
	InvitedBy       sql.NullInt64
	CreatedAt       int64
	ExpiresAt       int64
	AcceptedAt      sql.NullInt64
	InviterName     string
	InviterInitials string
	InviterColour   string
}

func CreateInvitation(ctx context.Context, q Querier, email, role string, invitedBy int64, tokenHash []byte, expiresAt int64) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO invitations
		(email, role, invited_by, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, unixepoch(), ?)`, email, role, invitedBy, tokenHash, expiresAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func InvitationByTokenHash(ctx context.Context, q Querier, tokenHash []byte) (*Invitation, error) {
	var i Invitation
	err := q.QueryRowContext(ctx, `SELECT i.id, i.email, i.role, i.invited_by, i.created_at,
		i.expires_at, i.accepted_at, coalesce(u.name, ''), coalesce(u.initials, ''), coalesce(u.colour, '')
		FROM invitations i LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.token_hash = ?`, tokenHash).
		Scan(&i.ID, &i.Email, &i.Role, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt,
			&i.InviterName, &i.InviterInitials, &i.InviterColour)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

func ListPendingInvitations(ctx context.Context, q Querier, now int64) ([]*Invitation, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.email, i.role, i.invited_by, i.created_at,
		i.expires_at, i.accepted_at, coalesce(u.name, '')
		FROM invitations i LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.accepted_at IS NULL AND i.expires_at > ? ORDER BY i.created_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invitation
	for rows.Next() {
		var i Invitation
		if err := rows.Scan(&i.ID, &i.Email, &i.Role, &i.InvitedBy, &i.CreatedAt,
			&i.ExpiresAt, &i.AcceptedAt, &i.InviterName); err != nil {
			return nil, err
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}

// InvitationByID reads an invitation for the second half of acceptance, where
// the token is no longer in hand but the id is.
func InvitationByID(ctx context.Context, q Querier, id int64) (*Invitation, error) {
	var i Invitation
	err := q.QueryRowContext(ctx, `SELECT id, email, role, invited_by, created_at,
		expires_at, accepted_at FROM invitations WHERE id = ?`, id).
		Scan(&i.ID, &i.Email, &i.Role, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

// AcceptInvitation marks an invitation used and reports whether it was still
// unused, so two submissions of the same accept form cannot both go through.
func AcceptInvitation(ctx context.Context, q Querier, id int64) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE invitations SET accepted_at = unixepoch() WHERE id = ? AND accepted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReissueInvitation replaces the token and expiry of a pending invitation, which
// is what "resend" does: the old link stops working. It returns ErrNotFound when
// no pending invitation has that id, because the statement is the guard as well:
// without the check, resending an invitation that has already been accepted
// hands out a link whose hash was never stored and which opens nothing.
func ReissueInvitation(ctx context.Context, q Querier, id int64, tokenHash []byte, expiresAt int64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE invitations SET token_hash = ?, expires_at = ? WHERE id = ? AND accepted_at IS NULL`,
		tokenHash, expiresAt, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func DeleteInvitation(ctx context.Context, q Querier, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM invitations WHERE id = ?`, id)
	return err
}

type PasswordReset struct {
	ID        int64
	UserID    int64
	ExpiresAt int64
	UsedAt    sql.NullInt64
}

func CreatePasswordReset(ctx context.Context, q Querier, userID int64, tokenHash []byte, expiresAt int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO password_resets
		(user_id, token_hash, created_at, expires_at) VALUES (?, ?, unixepoch(), ?)`,
		userID, tokenHash, expiresAt)
	return err
}

func PasswordResetByTokenHash(ctx context.Context, q Querier, tokenHash []byte) (*PasswordReset, error) {
	var r PasswordReset
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, expires_at, used_at FROM password_resets WHERE token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.UserID, &r.ExpiresAt, &r.UsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// UsePasswordReset marks the token spent and reports whether it was unspent.
func UsePasswordReset(ctx context.Context, q Querier, id int64) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE password_resets SET used_at = unixepoch() WHERE id = ? AND used_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Scopes     string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
	ExpiresAt  sql.NullInt64
}

func CreateAPIToken(ctx context.Context, q Querier, userID int64, name string, hash []byte, scopes string) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO api_tokens
		(user_id, name, hash, scopes, created_at) VALUES (?, ?, ?, ?, unixepoch())`,
		userID, name, hash, scopes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func APITokenByHash(ctx context.Context, q Querier, hash []byte) (*APIToken, error) {
	var t APIToken
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, name, scopes, created_at, last_used_at, expires_at FROM api_tokens
		 WHERE hash = ? AND revoked_at IS NULL`, hash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func ListAPITokens(ctx context.Context, q Querier) ([]*APIToken, error) {
	return listAPITokens(ctx, q, 0)
}

func ListUserAPITokens(ctx context.Context, q Querier, userID int64) ([]*APIToken, error) {
	if userID <= 0 {
		return nil, ErrNotFound
	}
	return listAPITokens(ctx, q, userID)
}

func listAPITokens(ctx context.Context, q Querier, userID int64) ([]*APIToken, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, user_id, name, scopes, created_at, last_used_at, expires_at FROM api_tokens
		 WHERE revoked_at IS NULL AND (?=0 OR user_id=?) ORDER BY created_at DESC,id DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// TouchAPIToken records that a token was used, at most once a minute. The
// column says when a token was last seen, which one write a minute answers, and
// a busy agent should not cost a write per request to keep it current.
func TouchAPIToken(ctx context.Context, q Querier, id int64) error {
	_, err := q.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = unixepoch()
		WHERE id = ? AND (last_used_at IS NULL OR last_used_at < unixepoch() - 60)`, id)
	return err
}

func RevokeAPIToken(ctx context.Context, q Querier, id int64) error {
	_, err := q.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = unixepoch() WHERE id = ?`, id)
	return err
}

type Setting struct {
	Key       string
	ValueJSON string
	Secret    bool
	UpdatedAt int64
}

func GetSetting(ctx context.Context, q Querier, key string) (*Setting, error) {
	var s Setting
	err := q.QueryRowContext(ctx,
		`SELECT key, value_json, secret, updated_at FROM settings WHERE key = ?`, key).
		Scan(&s.Key, &s.ValueJSON, &s.Secret, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

func PutSetting(ctx context.Context, q Querier, key, valueJSON string, secret bool, updatedBy int64) error {
	var by any
	if updatedBy != 0 {
		by = updatedBy
	}
	_, err := q.ExecContext(ctx, `INSERT INTO settings (key, value_json, secret, updated_by, updated_at)
		VALUES (?, ?, ?, ?, unixepoch())
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, secret = excluded.secret,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		key, valueJSON, secret, by)
	return err
}

// InsertActivity records one mutation. before and after are JSON or empty, and
// via is what carried the change, "token:<name>" or "mcp:<client>", empty for a
// person at a form.
func InsertActivity(ctx context.Context, q Querier, actorKind, actorID, via, entity, entityID, action, before, after string) error {
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := q.ExecContext(ctx, `INSERT INTO activity
		(actor_kind, actor_id, via, entity, entity_id, action, before_json, after_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, unixepoch())`,
		actorKind, actorID, null(via), entity, entityID, action, null(before), null(after))
	return err
}
