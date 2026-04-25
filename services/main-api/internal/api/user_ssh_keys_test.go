package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/ssh"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/sshkeys"
)

// fakeSSHKeyStore is an in-memory implementation matching the
// api.SSHKeyStore interface. We don't call sshkeys.Repo's pgx-bound methods
// here because the validation logic is tested in package sshkeys directly.
type fakeSSHKeyStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID][]dbstore.SshKey // by user_id
}

func newFakeSSHKeyStore() *fakeSSHKeyStore {
	return &fakeSSHKeyStore{rows: map[uuid.UUID][]dbstore.SshKey{}}
}

func (f *fakeSSHKeyStore) Add(_ context.Context, userID uuid.UUID, label, pubkey string) (dbstore.SshKey, error) {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubkey))
	if err != nil {
		return dbstore.SshKey{}, sshkeys.ErrInvalidPubkey
	}
	fp := sshkeys.Fingerprint(parsed)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.rows[userID] {
		if k.Fingerprint == fp {
			return dbstore.SshKey{}, sshkeys.ErrDuplicate
		}
	}
	row := dbstore.SshKey{
		ID:          uuid.New(),
		UserID:      userID,
		Label:       label,
		Pubkey:      pubkey,
		Fingerprint: fp,
		CreatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.rows[userID] = append(f.rows[userID], row)
	return row, nil
}

func (f *fakeSSHKeyStore) List(_ context.Context, userID uuid.UUID) ([]dbstore.SshKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dbstore.SshKey, len(f.rows[userID]))
	copy(out, f.rows[userID])
	return out, nil
}

func (f *fakeSSHKeyStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.rows[userID]
	for i, k := range rows {
		if k.ID == id {
			f.rows[userID] = append(rows[:i], rows[i+1:]...)
			return nil
		}
	}
	return sshkeys.ErrNotFound
}

func generateRawPubkey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(sshPub))
}

func setupSSHKeyRig(t *testing.T) (http.Handler, *fakeUserStore, *fakeSSHKeyStore) {
	t.Helper()
	store := newFakeUserStore()
	keys := newFakeSSHKeyStore()
	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: time.Hour},
	}
	router := api.NewUserRouter(api.UserHandlers{
		Auth:    authH,
		SSHKeys: &api.UserSSHKeyHandlers{Keys: keys},
	}, store)
	if _, err := store.CreateUser(context.Background(), "alice@example.com", "longenough01"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return router, store, keys
}

func TestSSHKeys_AddListDelete(t *testing.T) {
	t.Parallel()
	router, _, keys := setupSSHKeyRig(t)
	cookie := loginAs(t, userTestRig{router: router}, "alice@example.com", "longenough01")

	pubkey := generateRawPubkey(t)
	addRR := sendWithCookie(t, router, http.MethodPost, "/api/v1/ssh-keys", map[string]string{
		"label":  "laptop",
		"pubkey": pubkey,
	}, cookie)
	if addRR.Code != http.StatusCreated {
		t.Fatalf("add: %d body=%s", addRR.Code, addRR.Body.String())
	}
	var addResp struct {
		SshKey api.SSHKeyView `json:"ssh_key"`
	}
	if err := json.Unmarshal(addRR.Body.Bytes(), &addResp); err != nil {
		t.Fatalf("decode add: %v", err)
	}

	listRR := sendWithCookie(t, router, http.MethodGet, "/api/v1/ssh-keys", nil, cookie)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: %d", listRR.Code)
	}
	var listResp struct {
		SshKeys []api.SSHKeyView `json:"ssh_keys"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.SshKeys) != 1 || listResp.SshKeys[0].Label != "laptop" {
		t.Fatalf("list: %+v", listResp.SshKeys)
	}

	delRR := sendWithCookie(t, router, http.MethodDelete, "/api/v1/ssh-keys/"+addResp.SshKey.ID.String(), nil, cookie)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", delRR.Code)
	}
	if len(keys.rows) > 0 {
		for _, rows := range keys.rows {
			if len(rows) != 0 {
				t.Fatalf("expected empty rows after delete, got %+v", rows)
			}
		}
	}
}

func TestSSHKeys_AddRejectsInvalid(t *testing.T) {
	t.Parallel()
	router, _, _ := setupSSHKeyRig(t)
	cookie := loginAs(t, userTestRig{router: router}, "alice@example.com", "longenough01")
	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/ssh-keys", map[string]string{
		"label":  "broken",
		"pubkey": "not-an-ssh-key",
	}, cookie)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHKeys_DeleteOtherUserKey_404(t *testing.T) {
	t.Parallel()
	router, store, keys := setupSSHKeyRig(t)

	// Seed bob + a key for bob via the in-memory store directly.
	bob, err := store.CreateUser(context.Background(), "bob@example.com", "longenough02")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	bobKey, err := keys.Add(context.Background(), bob.ID, "bob's", generateRawPubkey(t))
	if err != nil {
		t.Fatalf("seed bob key: %v", err)
	}

	aliceCookie := loginAs(t, userTestRig{router: router}, "alice@example.com", "longenough01")
	rr := sendWithCookie(t, router, http.MethodDelete, "/api/v1/ssh-keys/"+bobKey.ID.String(), nil, aliceCookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}

	// Bob's key must still be there.
	if rows, _ := keys.List(context.Background(), bob.ID); len(rows) != 1 {
		t.Fatalf("bob's key was leaked through, rows=%+v", rows)
	}
}

// Make sure the integration of the merge hook works end-to-end: the user has
// stored keys, the create body has its own, both flow into the dispatched
// CreateInstance.
func TestUserInstances_CreateMergesStoredSSHKeys(t *testing.T) {
	t.Parallel()

	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	store := newFakeUserStore()
	keys := newFakeSSHKeyStore()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {
		ID:     nodeID,
		Status: dbstore.NodeStatusOnline,
	}}}
	disp.setConnected(nodeID)

	admin := &api.InstanceHandlers{
		Instances:  insts,
		Nodes:      getter,
		Dispatcher: disp,
		ExtraSSHKeysForOwner: func(ctx context.Context, owner uuid.UUID) []string {
			rows, err := keys.List(ctx, owner)
			if err != nil {
				return nil
			}
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, r.Pubkey)
			}
			return out
		},
	}

	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: time.Hour},
	}
	router := api.NewUserRouter(api.UserHandlers{
		Auth:      authH,
		Instances: api.NewUserInstanceHandlers(admin),
	}, store)

	alice, err := store.CreateUser(context.Background(), "a@x.com", "longenough01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	storedKey := generateRawPubkey(t)
	if _, err := keys.Add(context.Background(), alice.ID, "stored", storedKey); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	cookie := loginAs(t, userTestRig{router: router}, "a@x.com", "longenough01")

	bodyKey := generateRawPubkey(t)
	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":     nodeID.String(),
		"name":        "with-keys",
		"memory_mb":   1024,
		"vcpus":       1,
		"ssh_pubkeys": []string{bodyKey},
	}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.sent) != 1 {
		t.Fatalf("dispatch count: %d", len(disp.sent))
	}
	ci := disp.sent[0].Msg.GetCreateInstance()
	got := ci.SshPubkeys
	want := map[string]bool{bodyKey: false, storedKey: false}
	for _, k := range got {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("expected key %q in dispatch, got %v", k, got)
		}
	}
}

// sanity: Add returns ErrDuplicate on identical fingerprint
func TestSSHKeys_AddDuplicate(t *testing.T) {
	t.Parallel()
	router, _, _ := setupSSHKeyRig(t)
	cookie := loginAs(t, userTestRig{router: router}, "alice@example.com", "longenough01")
	pub := generateRawPubkey(t)
	body := map[string]string{"label": "laptop", "pubkey": pub}
	if rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/ssh-keys", body, cookie); rr.Code != http.StatusCreated {
		t.Fatalf("first add: %d", rr.Code)
	}
	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/ssh-keys", body, cookie)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// Sanity for the package-level error.
func TestSSHKeys_Errors_AreStable(t *testing.T) {
	t.Parallel()
	if !errors.Is(sshkeys.ErrNotFound, sshkeys.ErrNotFound) {
		t.Fatal("ErrNotFound should equal itself via errors.Is")
	}
}
