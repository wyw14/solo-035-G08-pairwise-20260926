package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyw14/solo-035/internal/approval"
	"github.com/wyw14/solo-035/internal/model"
)

// ErrApprovalRequired is returned when a critical director action is invoked
// directly without going through the two-person approval flow.
var ErrApprovalRequired = errors.New("this action requires a two-person approval ticket")

// restrictedActions are the critical director actions that may only run after
// two-person confirmation: stopping programs and driving backup paths.
var restrictedActions = map[string]bool{
	"stop-program":      true,
	"hold-event":        true,
	"mark-failed-over":  true,
	"schedule-failover": true,
	"cancel-failover":   true,
	"audio-failover":    true,
}

// IsRestrictedAction reports whether an action must pass through approvals.
func IsRestrictedAction(action string) bool {
	return restrictedActions[action]
}

// ApprovalProposalRequest is the initiator's explicit proposal: the event, the
// action, the exact parameters, and the generations they currently see.
type ApprovalProposalRequest struct {
	ControlRequest
	Initiator         string              `json:"initiator"`
	Expected          approval.Generations `json:"expected_generations"`
	TTLMillis         int64               `json:"ttl_millis,omitempty"`
}

// ApprovalDecisionRequest is the second operator's confirmation or refusal.
type ApprovalDecisionRequest struct {
	Approver string `json:"approver"`
	Approve  bool   `json:"approve"`
	Reason   string `json:"reason,omitempty"`
}

// ApprovalOutcome bundles the final ticket with the execution result when the
// decision was an approval.
type ApprovalOutcome struct {
	Ticket approval.Ticket `json:"ticket"`
	Result *ControlResult  `json:"result,omitempty"`
}

// ProposeControl validates a proposal against the live generations and opens a
// pending approval ticket. Older pending tickets for the same event and action
// are superseded.
func (runtime *Runtime) ProposeControl(request ApprovalProposalRequest) (approval.Ticket, error) {
	action := strings.TrimSpace(request.Action)
	if !IsRestrictedAction(action) {
		return approval.Ticket{}, fmt.Errorf("action %q is not subject to approval; use /api/control", action)
	}
	initiator := strings.TrimSpace(request.Initiator)
	if initiator == "" {
		return approval.Ticket{}, fmt.Errorf("approval initiator is required")
	}
	eventID := model.EventID(request.EventID)
	live, ok := runtime.Events.Get(eventID)
	if !ok {
		return approval.Ticket{}, fmt.Errorf("event %s not found", eventID)
	}
	if detail, drifted := driftBetween(request.Expected, live); drifted {
		return approval.Ticket{}, fmt.Errorf("expected generations do not match event %s: %s is %v, expected %v",
			eventID, detail.Field, detail.Actual, detail.Expected)
	}
	control := request.ControlRequest
	control.Action = action
	control.EventID = request.EventID
	proposal := approval.Proposal{
		EventID:   eventID,
		Action:    action,
		Initiator: initiator,
		Expected:  request.Expected,
		Params:    snapshotControlParameters(control),
		TTL:       time.Duration(request.TTLMillis) * time.Millisecond,
	}
	return runtime.Approvals.Propose(proposal)
}

// DecideControl applies the second operator's decision. On approval the
// generations are fenced once more and the action executes with the original
// parameter snapshot. Every outcome — rejection, drift, execution success or
// failure — is recorded on the ticket.
func (runtime *Runtime) DecideControl(ctx context.Context, ticketID string, request ApprovalDecisionRequest) (ApprovalOutcome, error) {
	approver := strings.TrimSpace(request.Approver)
	if approver == "" {
		return ApprovalOutcome{}, fmt.Errorf("approving operator is required")
	}
	ticket, ok := runtime.Approvals.Get(ticketID)
	if !ok {
		return ApprovalOutcome{}, approval.ErrTicketNotFound
	}
	live, exists := runtime.Events.Get(ticket.EventID)
	if !exists {
		return ApprovalOutcome{}, fmt.Errorf("event %s no longer exists", ticket.EventID)
	}
	decision := approval.Decision{
		TicketID: ticketID,
		Actor:    approver,
		Approve:  request.Approve,
		Reason:   strings.TrimSpace(request.Reason),
	}
	ticket, err := runtime.Approvals.Decide(decision, live)
	if err != nil {
		// Decide returns a zero value on refusal; re-read so the caller sees
		// the terminal state (invalidated/expired/rejected) and its trail.
		if current, ok := runtime.Approvals.Get(ticketID); ok {
			ticket = current
		}
		return ApprovalOutcome{Ticket: ticket}, err
	}
	if ticket.Status == approval.StatusRejected {
		return ApprovalOutcome{Ticket: ticket}, nil
	}
	if ticket.Status != approval.StatusApproved {
		return ApprovalOutcome{Ticket: ticket}, fmt.Errorf("ticket %s is %s", ticketID, ticket.Status)
	}

	// Final fence: generations must not have moved between the registry check
	// and execution. If they did, record the ticket as failed without running
	// anything.
	current, exists := runtime.Events.Get(ticket.EventID)
	if !exists {
		err = fmt.Errorf("event %s no longer exists", ticket.EventID)
		ticket, _ = runtime.Approvals.FinishExecution(ticketID, nil, err)
		return ApprovalOutcome{Ticket: ticket}, err
	}
	if detail, drifted := driftBetween(ticket.Expected, current); drifted {
		err = fmt.Errorf("live generations changed between approval and execution: %s is %v, expected %v",
			detail.Field, detail.Actual, detail.Expected)
		ticket, _ = runtime.Approvals.FinishExecution(ticketID, nil, err)
		return ApprovalOutcome{Ticket: ticket}, err
	}

	control := controlRequestFromSnapshot(ticket.Action, ticket.EventID, ticket.Parameters)
	result, execErr := runtime.runControl(ctx, control)
	if execErr != nil {
		ticket, _ = runtime.Approvals.FinishExecution(ticketID, nil, execErr)
		return ApprovalOutcome{Ticket: ticket, Result: &result}, execErr
	}
	ticket, _ = runtime.Approvals.FinishExecution(ticketID, result.Value, nil)
	return ApprovalOutcome{Ticket: ticket, Result: &result}, nil
}

// ListApprovals returns all approval tickets with current statuses.
func (runtime *Runtime) ListApprovals() []approval.Ticket {
	return runtime.Approvals.List()
}

// GetApproval returns one approval ticket.
func (runtime *Runtime) GetApproval(ticketID string) (approval.Ticket, bool) {
	return runtime.Approvals.Get(ticketID)
}

func driftBetween(expected approval.Generations, live model.LiveEvent) (approval.Detail, bool) {
	if expected.Revision != live.Revision {
		return approval.Detail{Field: "program_revision", Expected: string(expected.Revision), Actual: string(live.Revision)}, true
	}
	if expected.SourceGeneration != live.SourceGeneration {
		return approval.Detail{Field: "source_generation", Expected: expected.SourceGeneration, Actual: live.SourceGeneration}, true
	}
	if expected.CommandEpoch != live.CommandEpoch {
		return approval.Detail{Field: "command_epoch", Expected: expected.CommandEpoch, Actual: live.CommandEpoch}, true
	}
	return approval.Detail{}, false
}

func snapshotControlParameters(request ControlRequest) map[string]any {
	encoded, err := json.Marshal(request)
	if err != nil {
		return map[string]any{}
	}
	var params map[string]any
	if err := json.Unmarshal(encoded, &params); err != nil {
		return map[string]any{}
	}
	return params
}

func controlRequestFromSnapshot(action string, eventID model.EventID, params map[string]any) ControlRequest {
	encoded, err := json.Marshal(params)
	if err != nil {
		return ControlRequest{Action: action, EventID: string(eventID)}
	}
	var request ControlRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		return ControlRequest{Action: action, EventID: string(eventID)}
	}
	// Identity always comes from the approved ticket, never from the snapshot.
	request.Action = action
	request.EventID = string(eventID)
	return request
}
