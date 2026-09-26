package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wyw14/solo-035/internal/backup"
	"github.com/wyw14/solo-035/internal/model"
)

// DefaultApprovalTTL is the confirmation window when the initiator does not
// specify one.
const DefaultApprovalTTL = 2 * time.Minute

const (
	minApprovalTTL = 5 * time.Second
	maxApprovalTTL = 30 * time.Minute
)

// ProtectedActions must pass dual-control: an initiator submits event, action
// and expected generations, and a different confirmer approves before the
// action is executed.
var ProtectedActions = map[string]bool{
	"stop-program":      true,
	"audio-failover":    true,
	"schedule-failover": true,
	"hold-event":        true,
	"mark-failed-over":  true,
}

// RequiresApproval reports whether an action may only run after dual-control.
func RequiresApproval(action string) bool {
	return ProtectedActions[action]
}

// ApprovalSubmission is the initiator's request.
type ApprovalSubmission struct {
	EventID    string         `json:"event_id"`
	Action     string         `json:"action"`
	Parameters map[string]any `json:"parameters,omitempty"`
	Initiator  string         `json:"initiator"`
	TTLMillis  int64          `json:"ttl_millis,omitempty"`
}

// ApprovalDecision is the confirmer's answer.
type ApprovalDecision struct {
	Confirmer string `json:"confirmer"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
}

func liveGenerations(event model.LiveEvent) model.ExpectedGenerations {
	return model.ExpectedGenerations{
		ProgramRevision:  event.Revision,
		SourceGeneration: event.SourceGeneration,
		CommandEpoch:     event.CommandEpoch,
	}
}

// SubmitApproval pins the event, action, parameters and the generations the
// initiator currently sees. Nothing executes until a second operator confirms.
func (runtime *Runtime) SubmitApproval(submission ApprovalSubmission) (model.ApprovalRequest, error) {
	initiator := strings.TrimSpace(submission.Initiator)
	eventID := model.EventID(strings.TrimSpace(submission.EventID))
	draft := model.ApprovalRequest{
		ID:        model.NewApprovalID(),
		EventID:   eventID,
		Action:    strings.TrimSpace(submission.Action),
		Initiator: initiator,
	}
	if err := draft.ValidateSubmit(ProtectedActions, DefaultApprovalTTL); err != nil {
		return model.ApprovalRequest{}, err
	}
	event, ok := runtime.Events.Get(eventID)
	if !ok {
		return model.ApprovalRequest{}, fmt.Errorf("event %s not found", eventID)
	}
	parameters, err := normalizeParameters(draft.Action, submission.Parameters)
	if err != nil {
		return model.ApprovalRequest{}, err
	}
	draft.Parameters = parameters
	draft.EventName = event.Name
	draft.Expected = liveGenerations(event)
	if err := validateProtectedPrerequisites(runtime, draft, event); err != nil {
		return model.ApprovalRequest{}, err
	}
	ttl := DefaultApprovalTTL
	if submission.TTLMillis > 0 {
		ttl = time.Duration(submission.TTLMillis) * time.Millisecond
	}
	if ttl < minApprovalTTL || ttl > maxApprovalTTL {
		return model.ApprovalRequest{}, fmt.Errorf("ttl_millis must be between %d and %d", minApprovalTTL.Milliseconds(), maxApprovalTTL.Milliseconds())
	}
	draft.ExpiresAt = runtime.Clock().Add(ttl)
	return runtime.Approvals.Submit(draft)
}

// ListApprovals returns all approval requests, timing out expired ones and
// invalidating requests whose pinned generations drifted in the meantime.
func (runtime *Runtime) ListApprovals() []model.ApprovalRequest {
	runtime.sweepPendingApprovals()
	return runtime.Approvals.List()
}

// GetApproval returns one approval request, applying timeout and drift
// housekeeping first.
func (runtime *Runtime) GetApproval(id model.ApprovalID) (model.ApprovalRequest, error) {
	runtime.sweepPendingApprovals()
	request, ok := runtime.Approvals.Get(id)
	if !ok {
		return model.ApprovalRequest{}, model.ErrApprovalNotFound
	}
	return request, nil
}

// sweepPendingApprovals closes pending requests whose window elapsed or whose
// expected generations no longer match the live event, so stale approvals can
// never be confirmed even if the confirmer never looked at them again.
func (runtime *Runtime) sweepPendingApprovals() {
	runtime.Approvals.ExpirePending()
	for _, request := range runtime.Approvals.List() {
		if request.Status != model.ApprovalPending {
			continue
		}
		event, ok := runtime.Events.Get(request.EventID)
		if !ok {
			runtime.Approvals.Invalidate(request.ID, "", fmt.Sprintf("event %s no longer exists", request.EventID))
			continue
		}
		if drift := request.Expected.Match(liveGenerations(event)); drift != nil {
			runtime.Approvals.Invalidate(request.ID, "", drift.Error())
		}
	}
}

// DecideApproval records the confirmer's answer. On approval it re-checks that
// the live generations still match the pinned ones, executes the action and
// records the real outcome. A drift invalidates the approval without executing.
func (runtime *Runtime) DecideApproval(ctx context.Context, id model.ApprovalID, decision ApprovalDecision) (model.ApprovalRequest, error) {
	confirmer := strings.TrimSpace(decision.Confirmer)
	if confirmer == "" {
		return model.ApprovalRequest{}, fmt.Errorf("confirmer is required")
	}
	switch strings.ToLower(strings.TrimSpace(decision.Decision)) {
	case "approve", "approved", "confirm", "confirmed":
	case "reject", "rejected", "deny", "denied":
		return runtime.rejectApproval(id, confirmer, strings.TrimSpace(decision.Reason))
	default:
		return model.ApprovalRequest{}, fmt.Errorf("decision must be approve or reject")
	}
	request, err := runtime.Approvals.Decide(id, confirmer, true, strings.TrimSpace(decision.Reason))
	if err != nil {
		return runtime.staleRequest(id, err)
	}

	// The confirmer answers against the live event: a revision, source
	// generation or command epoch change since submission voids the approval.
	event, ok := runtime.Events.Get(request.EventID)
	if !ok {
		return runtime.voidConfirmed(request, confirmer, fmt.Sprintf("event %s no longer exists", request.EventID))
	}
	if drift := request.Expected.Match(liveGenerations(event)); drift != nil {
		return runtime.voidConfirmed(request, confirmer, drift.Error())
	}

	result, execErr := runtime.executeControl(ctx, controlRequestFromApproval(request))
	recorded, recordErr := runtime.Approvals.RecordExecution(id, resultValue(result), execErr)
	if recordErr != nil {
		return request, recordErr
	}
	// An execution failure is a terminal, traceable outcome rather than a
	// transport error: the action ran and its result is recorded.
	return recorded, nil
}

// voidConfirmed invalidates a request in the narrow window after approval but
// before execution, returning the generations error that caused the void.
func (runtime *Runtime) voidConfirmed(request model.ApprovalRequest, confirmer, detail string) (model.ApprovalRequest, error) {
	invalidated, err := runtime.Approvals.InvalidateApproved(request.ID, confirmer, detail)
	if err != nil {
		return request, fmt.Errorf("%w: %s", model.ErrApprovalGenerations, detail)
	}
	return invalidated, fmt.Errorf("%w: %s", model.ErrApprovalGenerations, detail)
}

func (runtime *Runtime) rejectApproval(id model.ApprovalID, confirmer, reason string) (model.ApprovalRequest, error) {
	if reason == "" {
		reason = "rejected by confirmer"
	}
	request, err := runtime.Approvals.Decide(id, confirmer, false, reason)
	if err != nil {
		return runtime.staleRequest(id, err)
	}
	return request, nil
}

// staleRequest attaches the stored request (for example the one the registry
// just timed out) to a failed decision, while keeping the error.
func (runtime *Runtime) staleRequest(id model.ApprovalID, decideErr error) (model.ApprovalRequest, error) {
	request, ok := runtime.Approvals.Get(id)
	if !ok {
		return model.ApprovalRequest{}, decideErr
	}
	return request, decideErr
}

func resultValue(result ControlResult) any {
	if result.Value == nil {
		return map[string]any{"action": result.Action, "status": result.Status}
	}
	return map[string]any{"action": result.Action, "status": result.Status, "value": result.Value}
}

func controlRequestFromApproval(request model.ApprovalRequest) ControlRequest {
	control := ControlRequest{Action: request.Action, EventID: string(request.EventID)}
	if delay, ok := request.Parameters["delay_millis"]; ok {
		switch value := delay.(type) {
		case float64:
			control.DelayMillis = int64(value)
		case int64:
			control.DelayMillis = value
		}
	}
	return control
}

// normalizeParameters rejects unknown parameters so the confirmed payload can
// never carry fields the initiator did not show.
func normalizeParameters(action string, parameters map[string]any) (map[string]any, error) {
	if len(parameters) == 0 {
		return nil, nil
	}
	normalized := make(map[string]any, len(parameters))
	for key, value := range parameters {
		switch key {
		case "delay_millis":
			if action != "schedule-failover" {
				return nil, fmt.Errorf("parameter %q is not valid for action %q", key, action)
			}
			number, ok := numberFromAny(value)
			if !ok || number < 0 {
				return nil, fmt.Errorf("parameter %q must be a non-negative number", key)
			}
			normalized[key] = number
		default:
			return nil, fmt.Errorf("unknown parameter %q for action %q", key, action)
		}
	}
	return normalized, nil
}

func numberFromAny(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), true
	case int64:
		return number, true
	case int:
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// validateProtectedPrerequisites gives the initiator immediate feedback when
// the requested action cannot run in the current state. Generations are still
// re-checked at confirmation time.
func validateProtectedPrerequisites(runtime *Runtime, draft model.ApprovalRequest, event model.LiveEvent) error {
	switch draft.Action {
	case "schedule-failover":
		decision, err := backup.Evaluate(event, 1, 0, runtime.Clock())
		if err != nil {
			return err
		}
		return decision.Validate(event)
	case "stop-program":
		if _, ok := runtime.MuxState.Get(event.ID); !ok {
			return fmt.Errorf("program for event %s does not exist", event.ID)
		}
	}
	return nil
}
