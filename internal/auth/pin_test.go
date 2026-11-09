package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ultraspace/internal/store"
)

func newTestAuth(t *testing.T) (*Auth, *store.Store, store.User) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	passHash, _ := bcrypt.GenerateFromPassword([]byte("testpassword123"), bcrypt.DefaultCost)
	u, err := st.CreateUser(context.Background(), "alice", string(passHash))
	if err != nil {
		t.Fatal(err)
	}

	a, err := New(st, Options{
		TTL:            time.Hour,
		SetupTokenPath: filepath.Join(t.TempDir(), "token"),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, st, u
}

func TestSetAndUnlockPIN(t *testing.T) {
	a, st, u := newTestAuth(t)
	mux := http.NewServeMux()
	a.Register(mux)

	// Create a session for alice
	token := randomToken(32)
	_ = st.CreateSession(context.Background(), hashToken(token), u.ID, time.Now().Add(time.Hour), "127.0.0.1", "test")

	setReq := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/account/pin", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	unlockReq := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/account/unlock", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	// Setting PIN with wrong password fails
	rec := setReq(map[string]any{"password": "wrong", "pin": "1234"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid PIN format (non-digits or too short)
	rec = setReq(map[string]any{"password": "testpassword123", "pin": "12a"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Set valid PIN
	rec = setReq(map[string]any{"password": "testpassword123", "pin": "123456"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify user now has PIN
	updatedUser, err := st.UserByID(context.Background(), u.ID)
	if err != nil || updatedUser.PINHash == "" {
		t.Fatalf("expected user to have pin_hash, got %v %v", updatedUser, err)
	}

	// Unlock with wrong PIN fails
	rec = unlockReq(map[string]any{"pin": "999999"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unlock with correct PIN succeeds
	rec = unlockReq(map[string]any{"pin": "123456"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unlock with master password fallback succeeds
	rec = unlockReq(map[string]any{"password": "testpassword123"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Clear PIN
	rec = setReq(map[string]any{"password": "testpassword123", "pin": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updatedUser, _ = st.UserByID(context.Background(), u.ID)
	if updatedUser.PINHash != "" {
		t.Fatalf("expected pin_hash to be cleared")
	}

	// Reset via ClearPINByName
	_ = st.SetPIN(context.Background(), u.ID, "somehash")
	ok, err := st.ClearPINByName(context.Background(), "alice")
	if err != nil || !ok {
		t.Fatalf("ClearPINByName failed: %v", err)
	}
	updatedUser, _ = st.UserByID(context.Background(), u.ID)
	if updatedUser.PINHash != "" {
		t.Fatalf("expected pin_hash to be cleared by name")
	}
}
