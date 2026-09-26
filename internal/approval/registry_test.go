package approval

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

type manualClock struct{ now time.Time }

func (clock *manualClock) Time() time.Time { return clock.now }

func (clock *manualClock) Advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

func testRequest(id model.ApprovalID, initiator string, now time.Time, ttl time.Duration) model.ApprovalRequest {
	return model.ApprovalRequest{
		ID:        id,
		EventID:   model.EventID("11111111-1111-1111-1111-111111111111"),
		EventName: "Evening News",
		Action:    "stop-program",
		Initiator: initiator,
		Expected: model.ExpectedGenerations{
			ProgramRevision:  model.RevisionID("22222222-2222-2222-2222-222222222222"),
			SourceGeneration: 1,
			CommandEpoch:     3,
		},
		SubmittedAt: now,
		ExpiresAt:   now.Add(ttl),
	}
}

func TestSubmitApproveAndRecordExecution(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(store, clock.Time)
	request, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != model.ApprovalPending || len(request.Timeline) != 1 {
		t.Fatalf("unexpected submitted request: %+v", request)
	}

	if _, err := registry.Decide(request.ID, "alice", true, ""); !errors.Is(err, model.ErrApprovalSameOperator) {
		t.Fatalf("same operator error = %v", err)
	}

	approved, err := registry.Decide(request.ID, "bob", true, "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != model.ApprovalApproved || approved.Confirmer != "bob" {
		t.Fatalf("unexpected approval: %+v", approved)
	}

	executed, err := registry.RecordExecution(request.ID, map[string]any{"event": request.EventID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Status != model.ApprovalExecuted || executed.ExecutedAt == nil {
		t.Fatalf("unexpected execution: %+v", executed)
	}
	if len(executed.Timeline) != 3 {
		t.Fatalf("timeline entries = %d", len(executed.Timeline))
	}

	entries, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("durable entries = %d", len(entries))
	}
}

func TestRejectionIsTerminalAndDurable(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	registry := NewRegistry(nil, clock.Time)
	request, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := registry.Decide(request.ID, "bob", false, "wrong program")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != model.ApprovalRejected || rejected.DecidedAt == nil {
		t.Fatalf("unexpected rejection: %+v", rejected)
	}
	if _, err := registry.Decide(request.ID, "carol", true, ""); !errors.Is(err, model.ErrApprovalNotPending) {
		t.Fatalf("deciding a rejected request error = %v", err)
	}
}

func TestDecisionAfterExpiryTimesOut(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	registry := NewRegistry(nil, clock.Time)
	request, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Second)
	if _, err := registry.Decide(request.ID, "bob", true, ""); !errors.Is(err, model.ErrApprovalNotPending) {
		t.Fatalf("late decision error = %v", err)
	}
	stored, ok := registry.Get(request.ID)
	if !ok || stored.Status != model.ApprovalExpired {
		t.Fatalf("status = %s", stored.Status)
	}
}

func TestExpirePendingSweepsLazyWindow(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	registry := NewRegistry(nil, clock.Time)
	if _, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(31 * time.Second)
	expired := registry.ExpirePending()
	if len(expired) != 1 || expired[0].Status != model.ApprovalExpired {
		t.Fatalf("expired = %+v", expired)
	}
	if got := registry.ExpirePending(); len(got) != 0 {
		t.Fatalf("second sweep expired %d again", len(got))
	}
}

func TestNewSubmissionInvalidatesEarlierPendingForEvent(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	registry := NewRegistry(nil, clock.Time)
	first, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Submit(testRequest(model.NewApprovalID(), "carol", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := registry.Get(first.ID)
	if !ok || stored.Status != model.ApprovalInvalidated {
		t.Fatalf("first request status = %s", stored.Status)
	}
	if second.Status != model.ApprovalPending {
		t.Fatalf("second request status = %s", second.Status)
	}
}

func TestExplicitInvalidationAndExecutionFailure(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	registry := NewRegistry(nil, clock.Time)
	request, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	invalidated, err := registry.Invalidate(request.ID, "bob", "revision drifted")
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.Status != model.ApprovalInvalidated {
		t.Fatalf("status = %s", invalidated.Status)
	}

	other, err := registry.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Decide(other.ID, "bob", true, ""); err != nil {
		t.Fatal(err)
	}
	failed, err := registry.RecordExecution(other.ID, nil, errors.New("backup encoder unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.ApprovalExecutionFailed || failed.ExecutedAt == nil {
		t.Fatalf("unexpected failure record: %+v", failed)
	}
}
func TestRecoverReplaysLogAndInterruptsApproved(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0).UTC()}
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	first := NewRegistry(store, clock.Time)
	request, err := first.Submit(testRequest(model.NewApprovalID(), "alice", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Decide(request.ID, "bob", true, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := first.Submit(testRequest(model.NewApprovalID(), "carol", clock.now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(filepath.Join(directory))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	second := NewRegistry(reopened, clock.Time)
	if err := second.Recover(entries); err != nil {
		t.Fatal(err)
	}
	confirmed, ok := second.Get(request.ID)
	if !ok || confirmed.Status != model.ApprovalInterrupted {
		t.Fatalf("confirmed request after restart = %+v", confirmed)
	}
	waiting, ok := second.Get(pending.ID)
	if !ok || waiting.Status != model.ApprovalPending {
		t.Fatalf("pending request after restart = %+v", waiting)
	}
}
