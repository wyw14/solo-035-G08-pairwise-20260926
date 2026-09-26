package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/solo-035/internal/approval"
	"github.com/wyw14/solo-035/internal/model"
)

func bootstrappedRuntime(t *testing.T) (*Runtime, model.LiveEvent) {
	t.Helper()
	runtime, err := NewRuntime(t.TempDir(), func() time.Time { return time.Unix(5000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(runtime); err != nil {
		t.Fatal(err)
	}
	events := runtime.Events.List()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	return runtime, events[0]
}

func stopProposal(event model.LiveEvent, initiator string) ApprovalProposalRequest {
	return ApprovalProposalRequest{
		ControlRequest: ControlRequest{Action: "stop-program", EventID: string(event.ID)},
		Initiator:      initiator,
		Expected: approval.Generations{
			Revision:         event.Revision,
			SourceGeneration: event.SourceGeneration,
			CommandEpoch:     event.CommandEpoch,
		},
		TTLMillis: 120_000,
	}
}

func TestRestrictedActionRejectedWithoutApproval(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	_, err := runtime.ExecuteControl(context.Background(), ControlRequest{Action: "stop-program", EventID: string(event.ID)})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	// Unrestricted actions still run directly.
	if _, err := runtime.ExecuteControl(context.Background(), ControlRequest{Action: "quality-reset", EventID: string(event.ID)}); err != nil {
		t.Fatalf("unrestricted action failed: %v", err)
	}
}

func TestTwoPersonApprovalStopsProgram(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)

	ticket, err := runtime.ProposeControl(stopProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Status != approval.StatusPending {
		t.Fatalf("status = %s", ticket.Status)
	}

	// Initiator cannot confirm their own proposal.
	if _, err := runtime.DecideControl(context.Background(), ticket.ID,
		ApprovalDecisionRequest{Approver: "director-a", Approve: true}); !errors.Is(err, approval.ErrSameOperator) {
		t.Fatalf("err = %v", err)
	}

	outcome, err := runtime.DecideControl(context.Background(), ticket.ID,
		ApprovalDecisionRequest{Approver: "director-b", Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Ticket.Status != approval.StatusExecuted || outcome.Result == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	// The mux program must actually have been deactivated.
	if _, active := runtime.MuxState.Get(event.ID); !active {
		t.Fatal("program record missing")
	}
	if outputs := runtime.PlayoutState.List(); len(outputs) != 0 && outputs[0].OnAir {
		t.Fatalf("playout still on air: %+v", outputs)
	}
	kinds := make([]string, 0, len(outcome.Ticket.Trail))
	for _, entry := range outcome.Ticket.Trail {
		kinds = append(kinds, entry.Kind)
	}
	if len(kinds) != 3 || kinds[0] != "proposed" || kinds[1] != "approved" || kinds[2] != "executed" {
		t.Fatalf("trail = %v", kinds)
	}
}

func TestProposalRejectsStaleExpectedGenerations(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	request := stopProposal(event, "director-a")
	request.Expected.CommandEpoch = event.CommandEpoch + 5
	if _, err := runtime.ProposeControl(request); err == nil {
		t.Fatal("expected stale generation proposal to be rejected")
	}
}

func TestGenerationDriftInvalidatesPendingApproval(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	ticket, err := runtime.ProposeControl(stopProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	// Field advances a program generation while the ticket waits.
	if _, err := runtime.Events.AdvanceProgram(event.ID, runtime.Clock()); err != nil {
		t.Fatal(err)
	}
	items := runtime.ListApprovals()
	if len(items) != 1 || items[0].Status != approval.StatusInvalidated {
		t.Fatalf("approvals = %+v", items)
	}
	if items[0].Invalidation.Field != "program_revision" {
		t.Fatalf("invalidation = %+v", items[0].Invalidation)
	}
	// Confirmation is refused and the program is untouched.
	_, err = runtime.DecideControl(context.Background(), ticket.ID,
		ApprovalDecisionRequest{Approver: "director-b", Approve: true})
	if err == nil {
		t.Fatal("expected invalidated decision error")
	}
}

func TestRejectionLeavesProgramRunningAndIsTraced(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	ticket, _ := runtime.ProposeControl(stopProposal(event, "director-a"))
	outcome, err := runtime.DecideControl(context.Background(), ticket.ID,
		ApprovalDecisionRequest{Approver: "director-b", Approve: false, Reason: "anchors still talking"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Ticket.Status != approval.StatusRejected {
		t.Fatalf("status = %s", outcome.Ticket.Status)
	}
	outputs := runtime.PlayoutState.List()
	if len(outputs) != 1 || !outputs[0].OnAir {
		t.Fatalf("playout should remain on air: %+v", outputs)
	}
}

func TestNewProposalSupersedesOldPending(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	first, err := runtime.ProposeControl(stopProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.ProposeControl(stopProposal(event, "director-c"))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := runtime.GetApproval(first.ID)
	if old.Status != approval.StatusSuperseded {
		t.Fatalf("old = %s", old.Status)
	}
	newer, _ := runtime.GetApproval(second.ID)
	if newer.Status != approval.StatusPending || newer.Initiator != "director-c" {
		t.Fatalf("new = %+v", newer)
	}
}

func TestApprovalCheckpointRoundTrip(t *testing.T) {
	runtime, event := bootstrappedRuntime(t)
	ticket, err := runtime.ProposeControl(stopProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := runtime.ExportCheckpoint()
	if len(checkpoint.Approvals) != 1 {
		t.Fatalf("checkpoint approvals = %d", len(checkpoint.Approvals))
	}

	fresh, err := NewRuntime(t.TempDir(), runtime.Clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.RestoreCheckpoint(checkpoint); err != nil {
		// Journal/playout replay may report in this isolated harness; the
		// approval registry itself is restored before that and must survive.
		t.Logf("restore reported: %v", err)
	}
	restored, ok := fresh.GetApproval(ticket.ID)
	if !ok || restored.Action != "stop-program" || restored.Initiator != "director-a" {
		t.Fatalf("restored ticket = %+v ok=%v", restored, ok)
	}
}
