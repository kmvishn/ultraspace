// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

// Session is one signed-in browser. ID is the row id, safe to show to the user.
type Session struct {
	ID        int64
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	IP        string
	UserAgent string
}

// State is a per-user JSON document (for example the open windows) and the device that last wrote it.
type State struct {
	Data       string
	Device     string
	DeviceName string
	UpdatedAt  time.Time
}

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS user_settings (
	user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	data    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS user_state (
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	key         TEXT NOT NULL,
	data        TEXT NOT NULL,
	device      TEXT NOT NULL,
	device_name TEXT NOT NULL,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (user_id, key)
);
`

// sessionColumns were added after the first release; migrate adds them to older databases.
var sessionColumns = []struct{ name, def string }{
	{"last_seen", "INTEGER NOT NULL DEFAULT 0"},
	{"ip", "TEXT NOT NULL DEFAULT ''"},
	{"user_agent", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range sessionColumns {
		if !have[c.name] {
			if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite writes serialized; the load here is tiny.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, hash string) (User, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, hash, now.Unix())
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, PasswordHash: hash, CreatedAt: now}, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username))
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`, id))
}

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.CreatedAt = time.Unix(created, 0)
	return u, err
}

func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time, ip, userAgent string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, now, expires.Unix(), now, ip, userAgent)
	return err
}

// SessionUser returns the user owning an unexpired session, and the session itself.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, Session, error) {
	var u User
	var sess Session
	var userCreated, created, expires, seen int64
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.password_hash, u.created_at,
		        s.rowid, s.created_at, s.expires_at, s.last_seen, s.ip, s.user_agent
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, time.Now().Unix()).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &userCreated,
			&sess.ID, &created, &expires, &seen, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, ErrNotFound
	}
	if err != nil {
		return User{}, Session{}, err
	}
	u.CreatedAt = time.Unix(userCreated, 0)
	sess.UserID = u.ID
	sess.CreatedAt, sess.ExpiresAt, sess.LastSeen = time.Unix(created, 0), time.Unix(expires, 0), time.Unix(seen, 0)
	return u, sess, nil
}

// TouchSession records activity and pushes the expiry forward (sliding sessions).
func (s *Store) TouchSession(ctx context.Context, id int64, expires time.Time, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?, last_seen = ?, ip = ? WHERE rowid = ?`,
		expires.Unix(), time.Now().Unix(), ip, id)
	return err
}

// Sessions lists a user's unexpired sessions, most recently active first.
func (s *Store) Sessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, created_at, expires_at, last_seen, ip, user_agent FROM sessions
		  WHERE user_id = ? AND expires_at > ? ORDER BY last_seen DESC`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created, expires, seen int64
		if err := rows.Scan(&sess.ID, &created, &expires, &seen, &sess.IP, &sess.UserAgent); err != nil {
			return nil, err
		}
		sess.UserID = userID
		sess.CreatedAt, sess.ExpiresAt, sess.LastSeen = time.Unix(created, 0), time.Unix(expires, 0), time.Unix(seen, 0)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteSessionByID removes one of the user's sessions. It reports ErrNotFound if there was none.
func (s *Store) DeleteSessionByID(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE rowid = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteOtherSessions signs the user out everywhere except session keep.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keep int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND rowid != ?`, userID, keep)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// Settings returns the user's settings JSON object, or "{}" when none are saved.
func (s *Store) Settings(ctx context.Context, userID int64) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = ?`, userID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	return data, err
}

func (s *Store) SetSettings(ctx context.Context, userID int64, data string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_settings (user_id, data) VALUES (?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET data = excluded.data`, userID, data)
	return err
}

// State returns the user's document stored under key, or ErrNotFound.
func (s *Store) State(ctx context.Context, userID int64, key string) (State, error) {
	var st State
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT data, device, device_name, updated_at FROM user_state WHERE user_id = ? AND key = ?`, userID, key).
		Scan(&st.Data, &st.Device, &st.DeviceName, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, ErrNotFound
	}
	st.UpdatedAt = time.UnixMilli(updated)
	return st, err
}

func (s *Store) SetState(ctx context.Context, userID int64, key string, st State) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_state (user_id, key, data, device, device_name, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, key) DO UPDATE SET data = excluded.data, device = excluded.device,
		   device_name = excluded.device_name, updated_at = excluded.updated_at`,
		userID, key, st.Data, st.Device, st.DeviceName, st.UpdatedAt.UnixMilli())
	return err
}

// DeleteState removes the user's document stored under key.
func (s *Store) DeleteState(ctx context.Context, userID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_state WHERE user_id = ? AND key = ?`, userID, key)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}
