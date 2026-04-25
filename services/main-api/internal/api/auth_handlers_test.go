package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/auth"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// --- fake UserStore --------------------------------------------------------

type fakeUserStore struct {
	mu       sync.Mutex
	users    map[string]dbstore.User // by email
	byID     map[uuid.UUID]dbstore.User
	sessions map[string]dbstore.Session // by raw token
	tokens   map[uuid.UUID]string       // userID -> last raw token (test convenience)
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:    map[string]dbstore.User{},
		byID:     map[uuid.UUID]dbstore.User{},
		sessions: map[string]dbstore.Session{},
		tokens:   map[uuid.UUID]string{},
	}
}

func (f *fakeUserStore) CreateUser(_ context.Context, email, password string) (dbstore.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	email = auth.NormalizeEmail(email)
	if err := auth.ValidateEmail(email); err != nil {
		return dbstore.User{}, err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return dbstore.User{}, err
	}
	if _, ok := f.users[email]; ok {
		return dbstore.User{}, auth.ErrEmailTaken
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return dbstore.User{}, err
	}
	u := dbstore.User{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: hash,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.users[email] = u
	f.byID[u.ID] = u
	return u, nil
}

func (f *fakeUserStore) Authenticate(_ context.Context, email, password string) (dbstore.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[auth.NormalizeEmail(email)]
	if !ok {
		return dbstore.User{}, auth.ErrInvalidCredentials
	}
	if err := auth.ComparePassword(u.PasswordHash, password); err != nil {
		return dbstore.User{}, auth.ErrInvalidCredentials
	}
	return u, nil
}

func (f *fakeUserStore) GetUser(_ context.Context, id uuid.UUID) (dbstore.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return dbstore.User{}, auth.ErrUserNotFound
}

func (f *fakeUserStore) CreateSession(_ context.Context, userID uuid.UUID, ttl time.Duration) (string, dbstore.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _, err := auth.GenerateSessionToken()
	if err != nil {
		return "", dbstore.Session{}, err
	}
	sess := dbstore.Session{
		ID:        uuid.New(),
		UserID:    userID,
		TokenHash: auth.HashSessionToken(raw),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
	}
	f.sessions[raw] = sess
	f.tokens[userID] = raw
	return raw, sess, nil
}

func (f *fakeUserStore) LookupSession(_ context.Context, rawToken string) (dbstore.Session, dbstore.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[rawToken]
	if !ok {
		return dbstore.Session{}, dbstore.User{}, auth.ErrSessionNotFound
	}
	if sess.ExpiresAt.Time.Before(time.Now()) {
		return dbstore.Session{}, dbstore.User{}, auth.ErrSessionNotFound
	}
	return sess, f.byID[sess.UserID], nil
}

func (f *fakeUserStore) RevokeSession(_ context.Context, rawToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, rawToken)
	return nil
}

// --- helpers ---------------------------------------------------------------

func newAuthRouter(t *testing.T, store *fakeUserStore, limiter api.LoginLimiter) http.Handler {
	t.Helper()
	authH := &api.AuthHandlers{
		Users:   store,
		Limiter: limiter,
		Config:  api.AuthConfig{SessionTTL: time.Hour},
	}
	return api.NewUserRouter(api.UserHandlers{Auth: authH}, store)
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func sessionCookie(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == api.SessionCookieName {
			return c
		}
	}
	return nil
}

// --- tests -----------------------------------------------------------------

func TestRegister_Success(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	rr := postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "alice@example.com",
		"password": "correct-horse-1",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if c := sessionCookie(rr); c == nil || c.HttpOnly != true || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie missing or misconfigured: %+v", c)
	}
}

func TestRegister_DuplicateEmail(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	body := map[string]string{"email": "dup@example.com", "password": "longenough01"}
	if rr := postJSON(t, router, "/api/v1/auth/register", body); rr.Code != http.StatusCreated {
		t.Fatalf("first register: %d", rr.Code)
	}
	rr := postJSON(t, router, "/api/v1/auth/register", body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_WeakPassword(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	rr := postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "w@example.com",
		"password": "short",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestLogin_Success(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "login@example.com",
		"password": "longenough01",
	})

	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "LOGIN@example.com",
		"password": "longenough01",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login: %d body=%s", rr.Code, rr.Body.String())
	}
	if sessionCookie(rr) == nil {
		t.Fatal("missing session cookie")
	}
}

func TestLogin_BadPasswordReturnsOpaque401(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)
	postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "u@example.com",
		"password": "longenough01",
	})

	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "u@example.com",
		"password": "WRONGenough00",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
	if sessionCookie(rr) != nil {
		t.Fatal("should not issue cookie on bad creds")
	}
}

func TestLogin_BadEmailReturnsSameMessage(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "no-such@example.com",
		"password": "longenough01",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
	// Body must not differentiate "user missing" from "wrong password".
	body := rr.Body.String()
	if !bytes.Contains([]byte(body), []byte("invalid email or password")) {
		t.Fatalf("expected opaque message, got %q", body)
	}
}

func TestLogin_RateLimited(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	limiter := auth.NewRateLimiter(2, time.Minute)
	router := newAuthRouter(t, store, limiter)
	postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "rl@example.com",
		"password": "longenough01",
	})

	for i := 0; i < 2; i++ {
		_ = postJSON(t, router, "/api/v1/auth/login", map[string]string{
			"email":    "rl@example.com",
			"password": "wrong-password",
		})
	}
	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "rl@example.com",
		"password": "longenough01",
	})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLogout_RevokesSession(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)
	postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "lo@example.com",
		"password": "longenough01",
	})
	loginRR := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "lo@example.com",
		"password": "longenough01",
	})
	cookie := sessionCookie(loginRR)
	if cookie == nil {
		t.Fatal("login did not issue cookie")
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutRR := httptest.NewRecorder()
	router.ServeHTTP(logoutRR, logoutReq)
	if logoutRR.Code != http.StatusNoContent {
		t.Fatalf("logout status: %d", logoutRR.Code)
	}

	// /me with the same cookie must now 401.
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.AddCookie(cookie)
	meRR := httptest.NewRecorder()
	router.ServeHTTP(meRR, meReq)
	if meRR.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", meRR.Code)
	}
}

func TestMe_RequiresAuth(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestMe_ReturnsUser(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := newAuthRouter(t, store, nil)
	postJSON(t, router, "/api/v1/auth/register", map[string]string{
		"email":    "me@example.com",
		"password": "longenough01",
	})
	loginRR := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "me@example.com",
		"password": "longenough01",
	})
	cookie := sessionCookie(loginRR)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.User.Email != "me@example.com" {
		t.Fatalf("email: %q", resp.User.Email)
	}
}

// Sanity check that fakeUserStore mirrors auth.Repo error semantics in case
// the production wiring is swapped to the real one.
func TestFakeUserStore_AuthenticateMatchesRealError(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	if _, err := store.Authenticate(context.Background(), "missing@example.com", "anything-long"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}
