package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ApprovalID identifies a dual-control approval request.
type ApprovalID string

func NewApprovalID() ApprovalID {
	return ApprovalID(uuid.NewString())
}

func (id ApprovalID) Valid() bool {
	return ValidateIdentity(string(id), "approval") == nil
}

// ApprovalStatus is the state of an approval request. Pending requests wait for
// a second operator; every other status is terminal.
type ApprovalStatus string

const (
	ApprovalPending         ApprovalStatus = "pending"
	ApprovalApproved        ApprovalStatus = "approved"
	ApprovalRejected        ApprovalStatus = "rejected"
	ApprovalExpired         ApprovalStatus = "expired"
	ApprovalInvalidated     ApprovalStatus = "invalidated"
	ApprovalExecuted        ApprovalStatus = "executed"
	ApprovalExecutionFailed ApprovalStatus = "execution-failed"
	// ApprovalInterrupted covers a confirmed request whose process restarted
	// before the action ran. It is terminal and must be resubmitted.
	ApprovalInterrupted ApprovalStatus = "interrupted"
)

func (status ApprovalStatus) String() string { return string(status) }

func (status ApprovalStatus) Terminal() bool {
	switch status {
	case ApprovalRejected, ApprovalExpired, ApprovalInvalidated, ApprovalExecuted, ApprovalExecutionFailed, ApprovalInterrupted:
		return true
	default:
		return false
	}
}

// ExpectedGenerations pins the event generations at submission time. The
// confirmation is only valid while the live event still matches every field.
type ExpectedGenerations struct {
	ProgramRevision  RevisionID `json:"program_revision"`
	SourceGeneration uint64     `json:"source_generation"`
	CommandEpoch     uint64     `json:"command_epoch"`
}

// Match reports whether the live generations still agree with what the
// initiator saw. The first drift is returned so the audit trail can name it.
func (expected ExpectedGenerations) Match(current ExpectedGenerations) error {
	if expected.ProgramRevision != current.ProgramRevision {
		return fmt.Errorf("program revision drifted: expected %s, live %s", expected.ProgramRevision, current.ProgramRevision)
	}
	if expected.SourceGeneration != current.SourceGeneration {
		return fmt.Errorf("source generation drifted: expected %d, live %d", expected.SourceGeneration, current.SourceGeneration)
	}
	if expected.CommandEpoch != current.CommandEpoch {
		return fmt.Errorf("command epoch drifted: expected %d, live %d", expected.CommandEpoch, current.CommandEpoch)
	}
	return nil
}

// ApprovalEvent is one immutable entry in the approval timeline.
type ApprovalEvent struct {
	At     time.Time      `json:"at"`
	Kind   string         `json:"kind"`
	Actor  string         `json:"actor,omitempty"`
	Status ApprovalStatus `json:"status"`
	Detail string         `json:"detail,omitempty"`
}

// ApprovalRequest is the persisted record of a dual-control request.
type ApprovalRequest struct {
	ID          ApprovalID          `json:"id"`
	EventID     EventID             `json:"event_id"`
	EventName   string              `json:"event_name"`
	Action      string              `json:"action"`
	Parameters  map[string]any      `json:"parameters,omitempty"`
	Expected    ExpectedGenerations `json:"expected_generations"`
	Initiator   string              `json:"initiator"`
	Confirmer   string              `json:"confirmer,omitempty"`
	Status      ApprovalStatus      `json:"status"`
	Reason      string              `json:"reason,omitempty"`
	SubmittedAt time.Time           `json:"submitted_at"`
	ExpiresAt   time.Time           `json:"expires_at"`
	DecidedAt   *time.Time          `json:"decided_at,omitempty"`
	ExecutedAt  *time.Time          `json:"executed_at,omitempty"`
	Result      any                 `json:"result,omitempty"`
	Timeline    []ApprovalEvent     `json:"timeline"`
}

var (
	ErrApprovalNotFound      = errors.New("approval request not found")
	ErrApprovalNotPending    = errors.New("approval request is not pending")
	ErrApprovalSameOperator  = errors.New("confirmer must be a different operator than the initiator")
	ErrApprovalGenerations   = errors.New("expected generations do not match the live event")
	ErrApprovalUnknownAction = errors.New("unknown protected action")
)

// ValidateSubmit checks the submission fields that do not depend on live state.
func (request *ApprovalRequest) ValidateSubmit(protected map[string]bool, ttl time.Duration) error {
	if request.EventID == "" || !request.EventID.Valid() {
		return errors.New("event_id is required")
	}
	if request.Action == "" {
		return errors.New("action is required")
	}
	if !protected[request.Action] {
		return fmt.Errorf("action %q is not eligible for approval", request.Action)
	}
	if strings.TrimSpace(request.Initiator) == "" {
		return errors.New("initiator is required")
	}
	if ttl <= 0 {
		return errors.New("approval time-to-live must be positive")
	}
	return nil
}
