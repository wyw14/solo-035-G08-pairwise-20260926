package approval

import (
	"fmt"
)

// ExportRecovery returns deep copies of every ticket for checkpoint export.
func (registry *Registry) ExportRecovery() []Ticket {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.sweepLocked()
	items := make([]Ticket, 0, len(registry.tickets))
	for _, ticket := range registry.tickets {
		items = append(items, cloneTicket(*ticket))
	}
	return items
}

// Restore rebuilds the registry from a checkpoint. Restored pending tickets
// are swept again against the live event set, so expired or drifted tickets
// settle into their terminal states immediately.
func (registry *Registry) Restore(tickets []Ticket) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	restored := make(map[string]*Ticket, len(tickets))
	for _, item := range tickets {
		if item.ID == "" {
			return fmt.Errorf("restored approval ticket is missing its identity")
		}
		if _, duplicate := restored[item.ID]; duplicate {
			return fmt.Errorf("restored approval ticket %s appears more than once", item.ID)
		}
		if item.EventID == "" || item.Action == "" || item.Initiator == "" {
			return fmt.Errorf("restored approval ticket %s is incomplete", item.ID)
		}
		if len(item.Trail) == 0 {
			return fmt.Errorf("restored approval ticket %s has no audit trail", item.ID)
		}
		copy := cloneTicket(item)
		restored[item.ID] = &copy
	}
	registry.tickets = restored
	registry.sweepLocked()
	return nil
}

func cloneTicket(ticket Ticket) Ticket {
	ticket.Parameters = cloneParams(ticket.Parameters)
	ticket.Trail = append([]Entry(nil), ticket.Trail...)
	if ticket.ExecutionResult != nil {
		result := make(map[string]any, len(ticket.ExecutionResult))
		for key, value := range ticket.ExecutionResult {
			result[key] = value
		}
		ticket.ExecutionResult = result
	}
	if ticket.Invalidation != nil {
		detail := *ticket.Invalidation
		ticket.Invalidation = &detail
	}
	if ticket.DecidedAt != nil {
		decided := *ticket.DecidedAt
		ticket.DecidedAt = &decided
	}
	if ticket.ClosedAt != nil {
		closed := *ticket.ClosedAt
		ticket.ClosedAt = &closed
	}
	return ticket
}
