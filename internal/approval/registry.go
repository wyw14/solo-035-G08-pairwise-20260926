package approval

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

// Registry keeps every approval request in memory and durably records each
// state change through the Store. All transitions are serialized so that two
// concurrent confirmers cannot execute the same approval twice.
type Registry struct {
	mu       sync.Mutex
	store    *Store
	clock    func() time.Time
	requests map[model.ApprovalID]model.ApprovalRequest
}

func NewRegistry(store *Store, clock func() time.Time) *Registry {
	if clock == nil {
		clock = time.Now
	}
	return &Registry{store: store, clock: clock, requests: make(map[model.ApprovalID]model.ApprovalRequest)}
}

// Submit records a new pending request and invalidates any earlier pending
// request for the same event and action: the newer parameters supersede them.
func (registry *Registry) Submit(request model.ApprovalRequest) (model.ApprovalRequest, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if request.ID == "" || !request.ID.Valid() {
		return model.ApprovalRequest{}, fmt.Errorf("approval identity is invalid")
	}
	if _, exists := registry.requests[request.ID]; exists {
		return model.ApprovalRequest{}, fmt.Errorf("approval %s already exists", request.ID)
	}
	now := registry.clock().UTC()
	request.Status = model.ApprovalPending
	request.SubmittedAt = now
	if request.ExpiresAt.IsZero() {
		return model.ApprovalRequest{}, fmt.Errorf("approval expiry is required")
	}
	request.ExpiresAt = request.ExpiresAt.UTC()
	request.Timeline = []model.ApprovalEvent{{At: now, Kind: "submitted", Actor: request.Initiator, Status: model.ApprovalPending}}
	if err := registry.appendLocked("submitted", request); err != nil {
		return model.ApprovalRequest{}, err
	}
	registry.requests[request.ID] = request

	// A newer request for the same event supersedes every outstanding
	// request: its pinned generations and parameters can no longer be trusted.
	for id, pending := range registry.requests {
		if id == request.ID || pending.Status != model.ApprovalPending || pending.EventID != request.EventID {
			continue
		}
		pending = transition(pending, model.ApprovalInvalidated, pending.Initiator,
			fmt.Sprintf("superseded by approval %s with newer parameters", request.ID), registry.clock().UTC())
		if err := registry.appendLocked("invalidated", pending); err != nil {
			return model.ApprovalRequest{}, err
		}
		registry.requests[id] = pending
	}
	return request, nil
}

// Decide applies a confirmer's decision. It refuses expired requests and
// records the rejection or the confirmation. Actual execution is finished by
// RecordExecution so that the action itself never runs under the registry lock.
func (registry *Registry) Decide(id model.ApprovalID, confirmer string, approve bool, reason string) (model.ApprovalRequest, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	request, err := registry.pendingLocked(id)
	if err != nil {
		return model.ApprovalRequest{}, err
	}
	now := registry.clock().UTC()
	if !request.ExpiresAt.IsZero() && !now.Before(request.ExpiresAt) {
		request = transition(request, model.ApprovalExpired, confirmer, "approval window closed before confirmation", now)
		if err := registry.appendLocked("expired", request); err != nil {
			return model.ApprovalRequest{}, err
		}
		registry.requests[id] = request
		return request, model.ErrApprovalNotPending
	}
	if confirmer == "" || confirmer == request.Initiator {
		return model.ApprovalRequest{}, model.ErrApprovalSameOperator
	}
	if !approve {
		request.Confirmer = confirmer
		request.Reason = reason
		request = transition(request, model.ApprovalRejected, confirmer, reason, now)
		if err := registry.appendLocked("rejected", request); err != nil {
			return model.ApprovalRequest{}, err
		}
		registry.requests[id] = request
		return request, nil
	}
	request.Confirmer = confirmer
	if reason != "" {
		request.Reason = reason
	}
	request = transition(request, model.ApprovalApproved, confirmer, reason, now)
	if err := registry.appendLocked("approved", request); err != nil {
		return model.ApprovalRequest{}, err
	}
	registry.requests[id] = request
	return request, nil
}

// Invalidate closes one pending request with an explicit reason, for example
// when the live generations drifted before the confirmer answered.
func (registry *Registry) Invalidate(id model.ApprovalID, actor, detail string) (model.ApprovalRequest, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	request, err := registry.pendingLocked(id)
	if err != nil {
		return model.ApprovalRequest{}, err
	}
	return registry.invalidateLocked(request, actor, detail)
}

// InvalidateApproved voids a request that was confirmed but has not executed
// yet. It exists for the narrow window between approval and execution when the
// final generation re-check finds drift; executed requests stay terminal.
func (registry *Registry) InvalidateApproved(id model.ApprovalID, actor, detail string) (model.ApprovalRequest, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	request, ok := registry.requests[id]
	if !ok {
		return model.ApprovalRequest{}, model.ErrApprovalNotFound
	}
	if request.Status != model.ApprovalApproved {
		return model.ApprovalRequest{}, fmt.Errorf("%w: cannot invalidate %s request", model.ErrApprovalNotPending, request.Status)
	}
	return registry.invalidateLocked(request, actor, detail)
}

func (registry *Registry) invalidateLocked(request model.ApprovalRequest, actor, detail string) (model.ApprovalRequest, error) {
	request = transition(request, model.ApprovalInvalidated, actor, detail, registry.clock().UTC())
	if err := registry.appendLocked("invalidated", request); err != nil {
		return model.ApprovalRequest{}, err
	}
	registry.requests[request.ID] = request
	return request, nil
}

// RecordExecution attaches the real outcome of a confirmed action.
func (registry *Registry) RecordExecution(id model.ApprovalID, value any, actionErr error) (model.ApprovalRequest, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	request, ok := registry.requests[id]
	if !ok {
		return model.ApprovalRequest{}, model.ErrApprovalNotFound
	}
	if request.Status != model.ApprovalApproved {
		return model.ApprovalRequest{}, fmt.Errorf("%w: cannot record execution for %s", model.ErrApprovalNotPending, request.Status)
	}
	now := registry.clock().UTC()
	if actionErr != nil {
		request.Result = map[string]any{"error": actionErr.Error()}
		request = transition(request, model.ApprovalExecutionFailed, request.Confirmer, actionErr.Error(), now)
	} else {
		request.Result = value
		request = transition(request, model.ApprovalExecuted, request.Confirmer, "", now)
	}
	if err := registry.appendLocked(request.Status.String(), request); err != nil {
		return model.ApprovalRequest{}, err
	}
	registry.requests[id] = request
	return request, nil
}

// ExpirePending closes every pending request whose window has elapsed. It is
// invoked on read and decision paths so timeouts never depend on a timer.
func (registry *Registry) ExpirePending() []model.ApprovalRequest {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.clock().UTC()
	var expired []model.ApprovalRequest
	for id, request := range registry.requests {
		if request.Status != model.ApprovalPending || request.ExpiresAt.IsZero() || now.Before(request.ExpiresAt) {
			continue
		}
		request = transition(request, model.ApprovalExpired, "", "approval window elapsed without confirmation", now)
		if err := registry.appendLocked("expired", request); err != nil {
			continue
		}
		registry.requests[id] = request
		expired = append(expired, request)
	}
	return expired
}

// InvalidateEvent marks all pending requests for an event invalid, for example
// when the event's live generations drifted while confirmation was outstanding.
func (registry *Registry) InvalidateEvent(eventID model.EventID, detail string) []model.ApprovalRequest {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.clock().UTC()
	var invalidated []model.ApprovalRequest
	for id, request := range registry.requests {
		if request.Status != model.ApprovalPending || request.EventID != eventID {
			continue
		}
		request = transition(request, model.ApprovalInvalidated, "", detail, now)
		if err := registry.appendLocked("invalidated", request); err != nil {
			continue
		}
		registry.requests[id] = request
		invalidated = append(invalidated, request)
	}
	return invalidated
}

func (registry *Registry) Get(id model.ApprovalID) (model.ApprovalRequest, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	request, ok := registry.requests[id]
	return request, ok
}

func (registry *Registry) List() []model.ApprovalRequest {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	items := make([]model.ApprovalRequest, 0, len(registry.requests))
	for _, request := range registry.requests {
		items = append(items, request)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].SubmittedAt.Equal(items[j].SubmittedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].SubmittedAt.After(items[j].SubmittedAt)
	})
	return items
}

// Recover rebuilds the registry from durable entries. A request found in the
// transient approved state never reached its execution result, so it is marked
// interrupted: the operators must submit a fresh approval.
func (registry *Registry) Recover(entries []LogEntry) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.requests = make(map[model.ApprovalID]model.ApprovalRequest)
	for _, entry := range entries {
		registry.requests[entry.Request.ID] = entry.Request
	}
	now := registry.clock().UTC()
	for id, request := range registry.requests {
		if request.Status == model.ApprovalApproved {
			request = transition(request, model.ApprovalInterrupted, request.Confirmer,
				"process restarted between approval and execution", now)
			if err := registry.appendLocked("interrupted", request); err != nil {
				return err
			}
			registry.requests[id] = request
		}
	}
	return nil
}

func (registry *Registry) pendingLocked(id model.ApprovalID) (model.ApprovalRequest, error) {
	request, ok := registry.requests[id]
	if !ok {
		return model.ApprovalRequest{}, model.ErrApprovalNotFound
	}
	if request.Status != model.ApprovalPending {
		return model.ApprovalRequest{}, fmt.Errorf("%w: %s", model.ErrApprovalNotPending, request.Status)
	}
	return request, nil
}

func (registry *Registry) appendLocked(event string, request model.ApprovalRequest) error {
	if registry.store == nil {
		return nil
	}
	return registry.store.Append(LogEntry{LoggedAt: registry.clock().UTC(), Event: event, Request: request})
}

func transition(request model.ApprovalRequest, status model.ApprovalStatus, actor, detail string, at time.Time) model.ApprovalRequest {
	request.Status = status
	entry := model.ApprovalEvent{At: at.UTC(), Kind: status.String(), Actor: actor, Status: status, Detail: detail}
	request.Timeline = append(request.Timeline, entry)
	if status.Terminal() {
		decisionStatuses := map[model.ApprovalStatus]bool{
			model.ApprovalRejected: true, model.ApprovalExpired: true,
			model.ApprovalInvalidated: true, model.ApprovalInterrupted: true,
		}
		if decisionStatuses[status] {
			request.DecidedAt = &entry.At
		} else {
			request.ExecutedAt = &entry.At
		}
	}
	return request
}
