package approval

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

// Status is the lifecycle state of a two-person approval ticket.
type Status string

const (
	// StatusPending waits for the second operator's decision.
	StatusPending Status = "pending"
	// StatusApproved means the second operator confirmed and the action is
	// being (or has been) executed.
	StatusApproved Status = "approved"
	// StatusRejected means the second operator refused to confirm.
	StatusRejected Status = "rejected"
	// StatusExpired means nobody decided before the TTL elapsed.
	StatusExpired Status = "expired"
	// StatusInvalidated means live generations (or parameters) drifted and
	// the ticket can no longer match the operator's expectation.
	StatusInvalidated Status = "invalidated"
	// StatusSuperseded means a newer proposal for the same event and action
	// replaced this one.
	StatusSuperseded Status = "superseded"
	// StatusExecuted means the approved action ran successfully.
	StatusExecuted Status = "executed"
	// StatusFailed means the approved action ran but returned an error.
	StatusFailed Status = "failed"
)

// Terminal reports whether no further transition is possible.
func (status Status) Terminal() bool {
	switch status {
	case StatusRejected, StatusExpired, StatusInvalidated, StatusSuperseded, StatusExecuted, StatusFailed:
		return true
	default:
		return false
	}
}

// Generations pins the live generations the initiator saw. Approval is only
// honoured while the event still reports exactly these values.
type Generations struct {
	Revision         model.RevisionID `json:"program_revision"`
	SourceGeneration uint64           `json:"source_generation"`
	CommandEpoch     uint64           `json:"command_epoch"`
}

// Matches reports whether the live event still matches the proposal.
func (generations Generations) Matches(event model.LiveEvent) bool {
	return generations.Revision == event.Revision &&
		generations.SourceGeneration == event.SourceGeneration &&
		generations.CommandEpoch == event.CommandEpoch
}

// Detail describes why an invalidation happened.
type Detail struct {
	Field    string `json:"field"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
}

// Entry is one immutable audit trail record.
type Entry struct {
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Kind     string    `json:"kind"`
	From     Status    `json:"from,omitempty"`
	To       Status    `json:"to,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Detail   *Detail   `json:"detail,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Duration int64     `json:"duration_ms,omitempty"`
}

// Ticket is one two-person approval request and its full lifecycle trail.
type Ticket struct {
	ID               string                 `json:"id"`
	EventID          model.EventID          `json:"event_id"`
	Action           string                 `json:"action"`
	Parameters       map[string]any         `json:"parameters"`
	Expected         Generations            `json:"expected_generations"`
	Initiator        string                 `json:"initiator"`
	Approver         string                 `json:"approver,omitempty"`
	Status           Status                 `json:"status"`
	ProposedAt       time.Time              `json:"proposed_at"`
	ExpiresAt        time.Time              `json:"expires_at"`
	DecidedAt        *time.Time             `json:"decided_at,omitempty"`
	ClosedAt         *time.Time             `json:"closed_at,omitempty"`
	Invalidation     *Detail                `json:"invalidation,omitempty"`
	ExecutionResult  map[string]any         `json:"execution_result,omitempty"`
	ExecutionError   string                 `json:"execution_error,omitempty"`
	Trail            []Entry                `json:"trail"`
}

// Proposal carries the fields an initiator must specify explicitly.
type Proposal struct {
	EventID   model.EventID
	Action    string
	Initiator string
	Expected  Generations
	Params    map[string]any
	TTL       time.Duration
}

// Decision is the second operator's confirmation or refusal.
type Decision struct {
	TicketID string
	Actor    string
	Approve  bool
	Reason   string
}

// ExecutionResult is what the gated action reported back.
type ExecutionResult struct {
	Value any
}

var (
	ErrTicketNotFound = errors.New("approval ticket not found")
	ErrTicketClosed   = errors.New("approval ticket is no longer pending")
	ErrSameOperator   = errors.New("approver must differ from the initiator")
)

func newTicket(proposal Proposal, id string, now time.Time) (Ticket, error) {
	initiator := strings.TrimSpace(proposal.Initiator)
	if initiator == "" {
		return Ticket{}, fmt.Errorf("approval initiator is required")
	}
	if strings.TrimSpace(proposal.Action) == "" {
		return Ticket{}, fmt.Errorf("approval action is required")
	}
	if !proposal.EventID.Valid() {
		return Ticket{}, fmt.Errorf("approval event identity is invalid")
	}
	if !proposal.Expected.Revision.Valid() {
		return Ticket{}, fmt.Errorf("expected program revision is required")
	}
	if proposal.Expected.SourceGeneration == 0 || proposal.Expected.CommandEpoch == 0 {
		return Ticket{}, fmt.Errorf("expected source generation and command epoch are required")
	}
	if proposal.TTL <= 0 {
		return Ticket{}, fmt.Errorf("approval time-to-live must be positive")
	}
	ticket := Ticket{
		ID:         id,
		EventID:    proposal.EventID,
		Action:     proposal.Action,
		Parameters: cloneParams(proposal.Params),
		Expected:   proposal.Expected,
		Initiator:  initiator,
		Status:     StatusPending,
		ProposedAt: now.UTC(),
		ExpiresAt:  now.Add(proposal.TTL).UTC(),
		Trail: []Entry{{
			At:    now.UTC(),
			Actor: initiator,
			Kind:  "proposed",
			To:    StatusPending,
		}},
	}
	return ticket, nil
}

func (ticket *Ticket) append(entry Entry) {
	entry.At = entry.At.UTC()
	ticket.Trail = append(ticket.Trail, entry)
}

func cloneParams(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	clone := make(map[string]any, len(params))
	for key, value := range params {
		clone[key] = value
	}
	return clone
}
