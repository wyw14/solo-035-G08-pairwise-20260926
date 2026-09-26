package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

type mutableClock struct{ now time.Time }

func (clock *mutableClock) Time() time.Time { return clock.now }

func (clock *mutableClock) Advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

func bootstrappedRuntime(t *testing.T, clock *mutableClock) (*Runtime, model.EventID) {
	t.Helper()
	runtime, err := NewRuntime(t.TempDir(), clock.Time)
	if err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(runtime); err != nil {
		t.Fatal(err)
	}
	operations := runtime.Operations()
	if len(operations) != 1 {
		t.Fatalf("operations = %d", len(operations))
	}
	return runtime, operations[0].EventID
}

func TestDualControlHappyPathStopsProgram(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)
	ctx := context.Background()

	// The protected action cannot run through the ordinary control endpoint.
	if _, err := runtime.ExecuteControl(ctx, ControlRequest{Action: "stop-program", EventID: string(eventID)}); err == nil {
		t.Fatal("stop-program executed without approval")
	} else {
		var protected ProtectedActionError
		if !errors.As(err, &protected) {
			t.Fatalf("expected ProtectedActionError, got %v", err)
		}
	}
	if output, ok := runtime.Playout.Output(eventID); !ok || !output.OnAir {
		t.Fatal("program should still be on air after rejected direct control")
	}

	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID:   string(eventID),
		Action:    "stop-program",
		Initiator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != model.ApprovalPending {
		t.Fatalf("status = %s", request.Status)
	}
	if request.Expected.CommandEpoch == 0 || request.Expected.ProgramRevision == "" {
		t.Fatalf("expected generations were not pinned: %+v", request.Expected)
	}

	// The initiator cannot confirm their own request.
	if _, err := runtime.DecideApproval(ctx, request.ID, ApprovalDecision{Confirmer: "alice", Decision: "approve"}); !errors.Is(err, model.ErrApprovalSameOperator) {
		t.Fatalf("self-confirmation error = %v", err)
	}

	executed, err := runtime.DecideApproval(ctx, request.ID, ApprovalDecision{Confirmer: "bob", Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Status != model.ApprovalExecuted || executed.Confirmer != "bob" || executed.ExecutedAt == nil {
		t.Fatalf("unexpected executed approval: %+v", executed)
	}
	if output, ok := runtime.Playout.Output(eventID); !ok || output.OnAir {
		t.Fatal("program should have stopped after dual-control approval")
	}
	if len(executed.Timeline) != 3 || executed.Timeline[0].Kind != "submitted" || executed.Timeline[2].Kind != "executed" {
		t.Fatalf("timeline = %+v", executed.Timeline)
	}
}

func TestRejectionLeavesActionUntouchedAndIsTraceable(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)

	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "hold-event", Initiator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{
		Confirmer: "bob", Decision: "reject", Reason: "not yet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != model.ApprovalRejected || rejected.Reason != "not yet" || rejected.DecidedAt == nil {
		t.Fatalf("unexpected rejection: %+v", rejected)
	}
	if event, _ := runtime.Events.Get(eventID); event.Phase != model.PhaseOnAir {
		t.Fatalf("phase changed despite rejection: %s", event.Phase)
	}
	// A rejected approval cannot be approved afterwards.
	if _, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{Confirmer: "carol", Decision: "approve"}); !errors.Is(err, model.ErrApprovalNotPending) {
		t.Fatalf("approve after reject error = %v", err)
	}
}

func TestExpiredApprovalCannotExecute(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)

	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "hold-event", Initiator: "alice", TTLMillis: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Second)
	expired, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{Confirmer: "bob", Decision: "approve"})
	if err == nil {
		t.Fatal("late confirmation succeeded")
	}
	if expired.Status != model.ApprovalExpired || expired.DecidedAt == nil {
		t.Fatalf("unexpected expired record: %+v", expired)
	}
	if event, _ := runtime.Events.Get(eventID); event.Phase != model.PhaseOnAir {
		t.Fatalf("phase changed despite timeout: %s", event.Phase)
	}
}

func TestRevisionDriftInvalidatesOutstandingApproval(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)

	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "hold-event", Initiator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The program advances while the approval is waiting: its pinned revision
	// is now stale.
	if _, err := runtime.Events.AdvanceProgram(eventID, clock.Time()); err != nil {
		t.Fatal(err)
	}

	// Sweeping via the list view invalidates the stale request immediately.
	listed := runtime.ListApprovals()
	if len(listed) != 1 || listed[0].Status != model.ApprovalInvalidated {
		t.Fatalf("listed approval = %+v", listed)
	}

	// Confirmation is refused and the phase never changes.
	invalidated, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{Confirmer: "bob", Decision: "approve"})
	if !errors.Is(err, model.ErrApprovalNotPending) {
		t.Fatalf("decision error = %v", err)
	}
	if invalidated.Status != model.ApprovalInvalidated {
		t.Fatalf("status = %s", invalidated.Status)
	}
	if event, _ := runtime.Events.Get(eventID); event.Phase != model.PhaseOnAir {
		t.Fatalf("phase changed on stale approval: %s", event.Phase)
	}
}

func TestConfirmedExecutionFailureIsRecorded(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)

	// No healthy backup encoder: the failover action is valid to request but
	// must fail when it actually runs.
	if !runtime.EncoderFlow.SetHealth("encoder-backup-1", false) {
		t.Fatal("backup encoder missing")
	}
	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID:    string(eventID),
		Action:     "schedule-failover",
		Initiator:  "alice",
		Parameters: map[string]any{"delay_millis": float64(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{Confirmer: "bob", Decision: "approve"})
	if err != nil {
		t.Fatalf("execution failure should be a recorded outcome, not error: %v", err)
	}
	if failed.Status != model.ApprovalExecutionFailed {
		t.Fatalf("status = %s", failed.Status)
	}
	last := failed.Timeline[len(failed.Timeline)-1]
	if last.Kind != "execution-failed" || last.Detail == "" {
		t.Fatalf("failure timeline = %+v", last)
	}
	if result, ok := failed.Result.(map[string]any); !ok || result["error"] == nil {
		t.Fatalf("failure result = %+v", failed.Result)
	}
}

func TestSubmitRejectsUnknownParametersAndUnprotectedAction(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	runtime, eventID := bootstrappedRuntime(t, clock)

	if _, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "hold-event", Initiator: "alice",
		Parameters: map[string]any{"delay_millis": float64(0)},
	}); err == nil {
		t.Fatal("unexpected parameter was accepted")
	}
	if _, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "adjust-bitrate", Initiator: "alice",
	}); err == nil {
		t.Fatal("unprotected action was accepted for approval")
	}
	if _, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "hold-event", Initiator: "alice", TTLMillis: 1,
	}); err == nil {
		t.Fatal("ttl below minimum was accepted")
	}
}

func TestApprovalAuditTrailIsDurable(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	directory := t.TempDir()
	runtime, err := NewRuntime(directory, clock.Time)
	if err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(runtime); err != nil {
		t.Fatal(err)
	}
	eventID := runtime.Operations()[0].EventID

	request, err := runtime.SubmitApproval(ApprovalSubmission{
		EventID: string(eventID), Action: "stop-program", Initiator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DecideApproval(context.Background(), request.ID, ApprovalDecision{Confirmer: "bob", Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	entries, err := runtime.ApprovalStore.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("durable approval entries = %d", len(entries))
	}
	kinds := map[string]bool{}
	for _, entry := range entries {
		kinds[entry.Event] = true
	}
	if !kinds["submitted"] || !kinds["approved"] || !kinds["executed"] {
		t.Fatalf("audit kinds = %+v", kinds)
	}

	// A rebuilt runtime replays the terminal record.
	rebuilt, err := NewRuntime(directory, clock.Time)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := rebuilt.Approvals.Get(request.ID)
	if !ok || stored.Status != model.ApprovalExecuted {
		t.Fatalf("rebuilt approval = %+v", stored)
	}
}
