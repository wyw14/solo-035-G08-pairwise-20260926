package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wyw14/solo-035/internal/service"
)

func approvalServer(t *testing.T) (http.Handler, map[string]any) {
	t.Helper()
	runtime, err := service.NewRuntime(t.TempDir(), func() time.Time { return time.Unix(9000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(runtime); err != nil {
		t.Fatal(err)
	}
	operations := runtime.Operations()
	if len(operations) != 1 {
		t.Fatalf("operations = %d", len(operations))
	}
	var web = fstest.MapFS{"operations.html": &fstest.MapFile{Data: []byte("ok")}}
	event := map[string]any{
		"event_id":          string(operations[0].EventID),
		"program_revision":  string(operations[0].ProgramRevision),
		"source_generation": operations[0].SourceGeneration,
		"command_epoch":     operations[0].CommandEpoch,
	}
	return NewServer(runtime, web).Router(), event
}

func do(t *testing.T, handler http.Handler, method, path, operator string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	if operator != "" {
		request.Header.Set("X-Operator", operator)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	return recorder.Code, payload
}

func TestDirectRestrictedControlIsForbidden(t *testing.T) {
	handler, event := approvalServer(t)
	status, payload := do(t, handler, http.MethodPost, "/api/control", "director-a",
		map[string]any{"action": "stop-program", "event_id": event["event_id"]})
	if status != http.StatusForbidden {
		t.Fatalf("status = %d body = %v", status, payload)
	}
}

func TestApprovalFlowEndToEnd(t *testing.T) {
	handler, event := approvalServer(t)

	// Initiator proposes.
	status, payload := do(t, handler, http.MethodPost, "/api/approvals", "director-a", map[string]any{
		"action":     "stop-program",
		"event_id":   event["event_id"],
		"initiator":  "director-a",
		"ttl_millis": 120000,
		"expected_generations": map[string]any{
			"program_revision":  event["program_revision"],
			"source_generation": event["source_generation"],
			"command_epoch":     event["command_epoch"],
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("propose status = %d body = %v", status, payload)
	}
	ticketID, _ := payload["id"].(string)
	if payload["status"] != "pending" {
		t.Fatalf("ticket = %v", payload)
	}

	// Same operator cannot confirm.
	status, _ = do(t, handler, http.MethodPost, "/api/approvals/"+ticketID+"/decision", "director-a",
		map[string]any{"approve": true})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("self-approval status = %d", status)
	}

	// Second operator confirms; the action executes.
	status, payload = do(t, handler, http.MethodPost, "/api/approvals/"+ticketID+"/decision", "director-b",
		map[string]any{"approve": true})
	if status != http.StatusOK {
		t.Fatalf("decision status = %d body = %v", status, payload)
	}
	ticket, _ := payload["ticket"].(map[string]any)
	if ticket["status"] != "executed" {
		t.Fatalf("ticket status = %v", ticket["status"])
	}

	// Listing shows the executed ticket with its trail.
	status, payload = do(t, handler, http.MethodGet, "/api/approvals", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	approvals, _ := payload["approvals"].([]any)
	if len(approvals) != 1 {
		t.Fatalf("approvals = %d", len(approvals))
	}
}

func TestApprovalRejectionDoesNotExecute(t *testing.T) {
	handler, event := approvalServer(t)
	_, payload := do(t, handler, http.MethodPost, "/api/approvals", "director-a", map[string]any{
		"action":     "stop-program",
		"event_id":   event["event_id"],
		"initiator":  "director-a",
		"ttl_millis": 120000,
		"expected_generations": map[string]any{
			"program_revision":  event["program_revision"],
			"source_generation": event["source_generation"],
			"command_epoch":     event["command_epoch"],
		},
	})
	ticketID, _ := payload["id"].(string)
	status, payload := do(t, handler, http.MethodPost, "/api/approvals/"+ticketID+"/decision", "director-b",
		map[string]any{"approve": false, "reason": "not yet"})
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %v", status, payload)
	}
	ticket, _ := payload["ticket"].(map[string]any)
	if ticket["status"] != "rejected" {
		t.Fatalf("status = %v", ticket["status"])
	}
}
