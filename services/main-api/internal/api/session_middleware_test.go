package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/auth"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

type stubResolver struct {
	user dbstore.User
	err  error
}

func (s stubResolver) LookupSession(_ context.Context, _ string) (dbstore.Session, dbstore.User, error) {
	if s.err != nil {
		return dbstore.Session{}, dbstore.User{}, s.err
	}
	return dbstore.Session{}, s.user, nil
}

func TestLoadSession_ValidCookie_AttachesUser(t *testing.T) {
	t.Parallel()
	want := dbstore.User{ID: uuid.New(), Email: "a@b.c", CreatedAt: pgtype.Timestamptz{Valid: true}}
	resolver := stubResolver{user: want}

	var got dbstore.User
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := api.UserFromContext(r.Context())
		if ok {
			got = u
		}
		w.WriteHeader(http.StatusOK)
	})

	wrapped := api.LoadSession(resolver)(final)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: "raw-token"})
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if got.ID != want.ID {
		t.Fatalf("user not attached: got %+v want %+v", got, want)
	}
}

func TestLoadSession_NoCookie_NoUser(t *testing.T) {
	t.Parallel()
	resolver := stubResolver{user: dbstore.User{ID: uuid.New()}}
	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := api.UserFromContext(r.Context())
		called = ok
		w.WriteHeader(http.StatusOK)
	})

	wrapped := api.LoadSession(resolver)(final)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if called {
		t.Fatal("user should not be attached without cookie")
	}
}

func TestLoadSession_InvalidCookie_NoUser(t *testing.T) {
	t.Parallel()
	resolver := stubResolver{err: auth.ErrSessionNotFound}
	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := api.UserFromContext(r.Context())
		called = ok
	})
	wrapped := api.LoadSession(resolver)(final)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: "stale"})
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if called {
		t.Fatal("invalid cookie should not yield user")
	}
}

func TestRequireUser_401WithoutContext(t *testing.T) {
	t.Parallel()
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := api.RequireUser(final)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestRequireUser_PassesWithContext(t *testing.T) {
	t.Parallel()
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := api.RequireUser(final)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(api.WithUser(req.Context(), dbstore.User{ID: uuid.New()}))
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
}

// Ensure errors.Is-friendly auth lookup.
func TestLoadSession_ResolverErrorIsHandled(t *testing.T) {
	t.Parallel()
	resolver := stubResolver{err: errors.New("db down")}
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	wrapped := api.LoadSession(resolver)(final)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: "x"})
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status: %d", rr.Code)
	}
}
