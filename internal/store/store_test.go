package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Databases created by v0.2 have a sessions table without the device columns.
func TestMigrateOldSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE, password_hash TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE sessions (token_hash BLOB PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
INSERT INTO users VALUES (1, 'admin', 'x', 0);
INSERT INTO sessions VALUES (x'01', 1, 0, 4102444800);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	u, sess, err := st.SessionUser(ctx, []byte{1})
	if err != nil || u.Username != "admin" {
		t.Fatalf("old session lost: %v %v", u, err)
	}
	if err := st.TouchSession(ctx, sess.ID, time.Now().Add(time.Hour), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	list, err := st.Sessions(ctx, 1)
	if err != nil || len(list) != 1 || list[0].IP != "1.2.3.4" {
		t.Fatalf("sessions = %v %v", list, err)
	}
}
