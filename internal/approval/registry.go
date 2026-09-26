package approval

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/solo-035/internal/model"
)

// DefaultTTL applies when a proposal does not pin its own validity window.
const DefaultTTL = 2 * time.Minute

// Executor runs the action described by an approved ticket. It receives the
// exact parameter snapshot the initiator proposed.
type Executor func(action string, parameters map[string]any) (any, error)

// Registry stores approval tickets and drives their lifecycle. It is safe for
// concurrent use. Every transition appends an immutable trail entry so
// proposals, decisions, expiries, invalidations and execution results are all
// traceable.
type Registry struct {
	mu      sync.Mutex
	tickets map[string]*Ticket
	clock   func() time.Time
	events  func() []model.LiveEvent
}

// NewRegistry builds an empty registry. events returns the current live event
// set; it is consulted on every read so stale tickets expire and drifted
// tickets invalidate automatically.
func NewRegistry(clock func() time.Time, events func() []model.LiveEvent) *Registry {
	if clock == nil {
		clock = time.Now
	}
	if events == nil {
		events = func() []model.LiveEvent { return nil }
	}
	return &Registry{tickets: make(map[string]*Ticket), clock: clock, events: events}
}

// Propose opens a pending ticket. Any earlier still-pending ticket for the same
// event and action is superseded: once an operator re-proposes (with different
// parameters or generations) an older confirmation must never be executed.
func (registry *Registry) Propose(proposal Proposal) (Ticket, error) {
	if proposal.TTL <= 0 {
		proposal.TTL = DefaultTTL
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.clock()
	ticket, err := newTicket(proposal, uuid.NewString(), now)
	if err != nil {
		return Ticket{}, err
	}
	for _, existing := range registry.tickets {
		if existing.Status != StatusPending {
			continue
		}
		if existing.EventID == ticket.EventID && existing.Action == ticket.Action {
			existing.transition(StatusSuperseded, ticket.Initiator, now,
				fmt.Sprintf("superseded by newer proposal %s", ticket.ID), nil)
		}
	}
	stored := ticket
	registry.tickets[ticket.ID] = &stored
	return ticket, nil
}

// Decide records the second operator's decision. The decider must differ from
// the initiator. A rejection is terminal. An approval re-checks the live
// generations at the exact confirmation instant and moves the ticket to
// approved; the caller then executes and reports via FinishExecution.
func (registry *Registry) Decide(decision Decision, current model.LiveEvent) (Ticket, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.clock()
	ticket, err := registry.requirePending(decision.TicketID, now)
	if err != nil {
		return Ticket{}, err
	}
	actor := strings.TrimSpace(decision.Actor)
	if actor == "" {
		return Ticket{}, fmt.Errorf("approving operator is required")
	}
	if actor == ticket.Initiator {
		return Ticket{}, ErrSameOperator
	}
	ticket.Approver = actor
	if !decision.Approve {
		ticket.transition(StatusRejected, actor, now, decision.Reason, nil)
		return *ticket, nil
	}
	if detail, drifted := ticket.driftDetail(current); drifted {
		ticket.transition(StatusInvalidated, actor, now,
			"live generations changed before confirmation", detail)
		return *ticket, fmt.Errorf("approval %s invalidated: %s is %v, expected %v",
			ticket.ID, detail.Field, detail.Actual, detail.Expected)
	}
	ticket.transition(StatusApproved, actor, now, decision.Reason, nil)
	return *ticket, nil
}

// FinishExecution records the actual outcome of an approved action: executed on
// success, failed on error.
func (registry *Registry) FinishExecution(ticketID string, value any, execErr error) (Ticket, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.clock()
	ticket, ok := registry.tickets[ticketID]
	if !ok {
		return Ticket{}, ErrTicketNotFound
	}
	if ticket.Status != StatusApproved {
		return Ticket{}, fmt.Errorf("%w: ticket %s is %s", ErrTicketClosed, ticketID, ticket.Status)
	}
	if execErr != nil {
		ticket.ExecutionError = execErr.Error()
		ticket.transition(StatusFailed, ticket.Approver, now, "", nil)
		ticket.Trail[len(ticket.Trail)-1].Outcome = execErr.Error()
		return *ticket, execErr
	}
	ticket.ExecutionResult = normalizeValue(value)
	ticket.transition(StatusExecuted, ticket.Approver, now, "", nil)
	return *ticket, nil
}

// Get returns one ticket, applying expiry and drift sweeps first.
func (registry *Registry) Get(id string) (Ticket, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.sweepLocked()
	ticket, ok := registry.tickets[id]
	if !ok {
		return Ticket{}, false
	}
	return *ticket, true
}

// List returns all tickets, newest proposal first, after expiring stale and
// invalidating generation-drifted tickets.
func (registry *Registry) List() []Ticket {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.sweepLocked()
	items := make([]Ticket, 0, len(registry.tickets))
	for _, ticket := range registry.tickets {
		items = append(items, *ticket)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ProposedAt.After(items[j].ProposedAt) })
	return items
}

// Pending lists tickets still awaiting a decision, optionally for one event.
func (registry *Registry) Pending(eventID model.EventID) []Ticket {
	items := registry.List()
	pending := make([]Ticket, 0)
	for _, ticket := range items {
		if ticket.Status == StatusPending && (eventID == "" || ticket.EventID == eventID) {
			pending = append(pending, ticket)
		}
	}
	return pending
}

func (registry *Registry) requirePending(id string, now time.Time) (*Ticket, error) {
	ticket, ok := registry.tickets[id]
	if !ok {
		return nil, ErrTicketNotFound
	}
	if ticket.Status != StatusPending {
		return nil, fmt.Errorf("%w: ticket %s is %s", ErrTicketClosed, id, ticket.Status)
	}
	if !now.Before(ticket.ExpiresAt) {
		ticket.transition(StatusExpired, "system", now, "approval window elapsed", nil)
		return nil, fmt.Errorf("%w: ticket %s is %s", ErrTicketClosed, id, StatusExpired)
	}
	return ticket, nil
}

func (ticket *Ticket) driftDetail(current model.LiveEvent) (*Detail, bool) {
	if ticket.Expected.Revision != current.Revision {
		return &Detail{Field: "program_revision", Expected: string(ticket.Expected.Revision), Actual: string(current.Revision)}, true
	}
	if ticket.Expected.SourceGeneration != current.SourceGeneration {
		return &Detail{Field: "source_generation", Expected: ticket.Expected.SourceGeneration, Actual: current.SourceGeneration}, true
	}
	if ticket.Expected.CommandEpoch != current.CommandEpoch {
		return &Detail{Field: "command_epoch", Expected: ticket.Expected.CommandEpoch, Actual: current.CommandEpoch}, true
	}
	return nil, false
}

func (ticket *Ticket) transition(to Status, actor string, now time.Time, reason string, detail *Detail) {
	from := ticket.Status
	entry := Entry{At: now.UTC(), Actor: actor, Kind: string(to), From: from, To: to, Reason: reason, Detail: detail}
	if (to == StatusApproved || to == StatusRejected) && ticket.DecidedAt == nil {
		decided := now.UTC()
		ticket.DecidedAt = &decided
	}
	if to.Terminal() && ticket.ClosedAt == nil {
		closed := now.UTC()
		ticket.ClosedAt = &closed
		if to == StatusInvalidated && detail != nil {
			ticket.Invalidation = detail
		}
	}
	ticket.Status = to
	ticket.append(entry)
}

// sweepLocked expires stale tickets and invalidates pending tickets whose live
// generations have drifted. It must be called with the mutex held.
func (registry *Registry) sweepLocked() {
	now := registry.clock()
	current := make(map[model.EventID]model.LiveEvent)
	for _, event := range registry.events() {
		current[event.ID] = event
	}
	for _, ticket := range registry.tickets {
		if ticket.Status != StatusPending {
			continue
		}
		if !now.Before(ticket.ExpiresAt) {
			ticket.transition(StatusExpired, "system", now, "approval window elapsed", nil)
			continue
		}
		if event, ok := current[ticket.EventID]; ok {
			if detail, drifted := ticket.driftDetail(event); drifted {
				ticket.transition(StatusInvalidated, "system", now,
					"live generations changed while awaiting confirmation", detail)
			}
		}
	}
}

func normalizeValue(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{"value": value}
}
