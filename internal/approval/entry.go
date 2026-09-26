package approval

import (
	"time"

	"github.com/wyw14/solo-035/internal/model"
)

// LogEntry is one durable line in approvals.jsonl. Each entry carries the full
// request snapshot after a state change, so the register can be rebuilt by
// replaying the file and keeping the latest snapshot per approval ID.
type LogEntry struct {
	LoggedAt time.Time             `json:"logged_at"`
	Event    string                `json:"event"`
	Request  model.ApprovalRequest `json:"request"`
}
