package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

func TestHTTPMiddleware_RecordsDuration(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	c := NewCollectors(reg)

	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	wrapped := c.HTTPMiddleware("test")(final)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status: %d", rr.Code)
	}

	mr := httptest.NewRecorder()
	Handler(reg).ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mr.Body.String()
	if !strings.Contains(body, `api_request_duration_seconds_count{method="GET",route="test",status="418"}`) {
		t.Fatalf("missing recorded sample, body:\n%s", body)
	}
}

type fakeDomainQ struct {
	insts []dbstore.Instance
	slots []dbstore.ListSlotsForAdminViewRow
}

func (f *fakeDomainQ) ListInstances(_ context.Context, _ uuid.NullUUID) ([]dbstore.Instance, error) {
	return f.insts, nil
}
func (f *fakeDomainQ) ListSlotsForAdminView(_ context.Context) ([]dbstore.ListSlotsForAdminViewRow, error) {
	return f.slots, nil
}

func TestDomainRefresher_Sample(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	c := NewCollectors(reg)

	q := &fakeDomainQ{
		insts: []dbstore.Instance{
			{ID: uuid.New(), State: dbstore.InstanceStateRunning, CreatedAt: pgtype.Timestamptz{Valid: true}},
			{ID: uuid.New(), State: dbstore.InstanceStateRunning, CreatedAt: pgtype.Timestamptz{Valid: true}},
			{ID: uuid.New(), State: dbstore.InstanceStateStopped, CreatedAt: pgtype.Timestamptz{Valid: true}},
		},
		slots: []dbstore.ListSlotsForAdminViewRow{
			{ID: uuid.New(), NodeName: "h20a", SlotIndex: 0, Status: dbstore.SlotStatusInUse},
			{ID: uuid.New(), NodeName: "h20a", SlotIndex: 1, Status: dbstore.SlotStatusFree},
		},
	}
	r := &DomainRefresher{Queries: q, Coll: c}
	if err := r.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}

	mr := httptest.NewRecorder()
	Handler(reg).ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mr.Body.String()
	if !strings.Contains(body, `instance_total{state="running"} 2`) {
		t.Fatalf("missing running=2 in metrics:\n%s", body)
	}
	if !strings.Contains(body, `instance_total{state="stopped"} 1`) {
		t.Fatalf("missing stopped=1 in metrics:\n%s", body)
	}
	if !strings.Contains(body, `gpu_slot_used{node_name="h20a",slot_index="0"} 1`) {
		t.Fatalf("missing slot 0 in_use:\n%s", body)
	}
	if !strings.Contains(body, `gpu_slot_used{node_name="h20a",slot_index="1"} 0`) {
		t.Fatalf("missing slot 1 free:\n%s", body)
	}
}
