package approval

import (
	"errors"
	"testing"
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

type mutableClock struct{ now time.Time }

func (clock *mutableClock) get() time.Time { return clock.now }
func (clock *mutableClock) add(d time.Duration) {
	clock.now = clock.now.Add(d)
}

func fixtureEvent(t *testing.T, now time.Time) model.LiveEvent {
	t.Helper()
	event, err := model.NewLiveEvent("Evening News", now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func fixtureProposal(event model.LiveEvent, initiator string) Proposal {
	return Proposal{
		EventID:   event.ID,
		Action:    "stop-program",
		Initiator: initiator,
		Expected: Generations{
			Revision:         event.Revision,
			SourceGeneration: event.SourceGeneration,
			CommandEpoch:     event.CommandEpoch,
		},
		Params: map[string]any{"event_id": string(event.ID), "bitrate": 8000},
		TTL:    time.Minute,
	}
}

func TestTwoPersonApprovalExecutesWithSecondOperator(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Status != StatusPending || ticket.Approver != "" {
		t.Fatalf("new ticket = %+v", ticket)
	}

	ticket, err = registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Status != StatusApproved {
		t.Fatalf("status = %s", ticket.Status)
	}
	if ticket.Approver != "director-b" || ticket.DecidedAt == nil {
		t.Fatalf("approval metadata = %+v", ticket)
	}

	ticket, err = registry.FinishExecution(ticket.ID, map[string]any{"event_id": string(event.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Status != StatusExecuted || ticket.ExecutionResult["event_id"] != string(event.ID) {
		t.Fatalf("executed ticket = %+v", ticket)
	}
	kinds := trailKinds(ticket)
	if len(kinds) != 3 || kinds[0] != "proposed" || kinds[1] != "approved" || kinds[2] != "executed" {
		t.Fatalf("trail = %v", kinds)
	}
}

func TestSameOperatorCannotApprove(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-a", Approve: true}, event); !errors.Is(err, ErrSameOperator) {
		t.Fatalf("err = %v, want ErrSameOperator", err)
	}
	reloaded, ok := registry.Get(ticket.ID)
	if !ok || reloaded.Status != StatusPending {
		t.Fatalf("ticket should stay pending, got %+v", reloaded)
	}
}

func TestRejectionIsTerminalAndTraced(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: false, Reason: "talent still on air"}, event)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Status != StatusRejected || ticket.ClosedAt == nil {
		t.Fatalf("ticket = %+v", ticket)
	}
	last := ticket.Trail[len(ticket.Trail)-1]
	if last.Reason != "talent still on air" || last.Actor != "director-b" {
		t.Fatalf("rejection trail = %+v", last)
	}
	// A rejected ticket cannot be decided again.
	if _, err := registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event); !errors.Is(err, ErrTicketClosed) {
		t.Fatalf("err = %v, want ErrTicketClosed", err)
	}
}

func TestTicketExpiresAfterTTL(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(61 * time.Second)
	if items := registry.List(); len(items) != 1 || items[0].Status != StatusExpired {
		t.Fatalf("statuses = %+v", items)
	}
	// Deciding an expired ticket fails and stays expired.
	if _, err := registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event); !errors.Is(err, ErrTicketClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerationDriftInvalidatesPendingTicket(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	live := event
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{live} })

	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}

	// Program revision advances while the ticket is pending: the sweep must
	// invalidate it with the exact drift detail.
	clock.add(time.Second)
	live = event.NewProgram(clock.now)
	items := registry.List()
	if len(items) != 1 || items[0].Status != StatusInvalidated {
		t.Fatalf("statuses = %+v", items)
	}
	if items[0].Invalidation == nil || items[0].Invalidation.Field != "program_revision" {
		t.Fatalf("invalidation = %+v", items[0].Invalidation)
	}

	// Direct approval against stale generations is also refused.
	if _, err := registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event); err == nil {
		t.Fatal("expected invalidation error")
	}
}

func TestSourceGenerationDriftBlocksApproval(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })
	ticket, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	drifted := event.NewSource(clock.now)
	if _, err := registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, drifted); err == nil {
		t.Fatal("expected drift error")
	}
	reloaded, _ := registry.Get(ticket.ID)
	if reloaded.Status != StatusInvalidated || reloaded.Invalidation.Field != "source_generation" {
		t.Fatalf("ticket = %+v", reloaded)
	}
}

func TestNewProposalSupersedesEarlierPending(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	first, err := registry.Propose(fixtureProposal(event, "director-a"))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(time.Second)
	second, err := registry.Propose(fixtureProposal(event, "director-c"))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := registry.Get(first.ID)
	if old.Status != StatusSuperseded {
		t.Fatalf("old ticket = %s", old.Status)
	}
	newer, _ := registry.Get(second.ID)
	if newer.Status != StatusPending {
		t.Fatalf("new ticket = %s", newer.Status)
	}
	// The superseded ticket cannot be confirmed.
	if _, err := registry.Decide(Decision{TicketID: first.ID, Actor: "director-b", Approve: true}, event); !errors.Is(err, ErrTicketClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedExecutionIsRecorded(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })
	ticket, _ := registry.Propose(fixtureProposal(event, "director-a"))
	ticket, _ = registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event)

	ticket, err := registry.FinishExecution(ticket.ID, nil, errors.New("mux refused stop"))
	if err == nil {
		t.Fatal("expected execution error to surface")
	}
	if ticket.Status != StatusFailed || ticket.ExecutionError != "mux refused stop" {
		t.Fatalf("ticket = %+v", ticket)
	}
	if ticket.Trail[len(ticket.Trail)-1].Outcome != "mux refused stop" {
		t.Fatalf("trail outcome = %+v", ticket.Trail[len(ticket.Trail)-1])
	}
}

func TestProposalRequiresExplicitGenerations(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })

	proposal := fixtureProposal(event, "director-a")
	proposal.Expected.CommandEpoch = 0
	if _, err := registry.Propose(proposal); err == nil {
		t.Fatal("expected missing generation error")
	}
	proposal = fixtureProposal(event, "director-a")
	proposal.Initiator = "  "
	if _, err := registry.Propose(proposal); err == nil {
		t.Fatal("expected missing initiator error")
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	clock := &mutableClock{now: time.Unix(1000, 0).UTC()}
	event := fixtureEvent(t, clock.now)
	registry := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })
	ticket, _ := registry.Propose(fixtureProposal(event, "director-a"))
	ticket, _ = registry.Decide(Decision{TicketID: ticket.ID, Actor: "director-b", Approve: true}, event)
	ticket, _ = registry.FinishExecution(ticket.ID, "ok", nil)

	exported := registry.ExportRecovery()
	if len(exported) != 1 {
		t.Fatalf("exported = %d", len(exported))
	}
	fresh := NewRegistry(clock.get, func() []model.LiveEvent { return []model.LiveEvent{event} })
	if err := fresh.Restore(exported); err != nil {
		t.Fatal(err)
	}
	restored, ok := fresh.Get(ticket.ID)
	if !ok || restored.Status != StatusExecuted || restored.Approver != "director-b" {
		t.Fatalf("restored = %+v ok=%v", restored, ok)
	}
	if restored.Parameters["bitrate"] != 8000 {
		t.Fatalf("parameters lost: %+v", restored.Parameters)
	}
	if len(restored.Trail) != 3 {
		t.Fatalf("trail lost: %+v", restored.Trail)
	}
}

func trailKinds(ticket Ticket) []string {
	kinds := make([]string, 0, len(ticket.Trail))
	for _, entry := range ticket.Trail {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}
