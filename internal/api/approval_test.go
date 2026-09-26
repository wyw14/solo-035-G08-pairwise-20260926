package api

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/wyw14/solo-035/internal/model"
	"github.com/wyw14/solo-035/internal/service"
)

func approvalServer(t *testing.T) (*Server, model.EventID) {
	t.Helper()
	runtime, err := service.NewRuntime(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(runtime); err != nil {
		t.Fatal(err)
	}
	var web fs.FS = fstest.MapFS{"approvals.html": &fstest.MapFile{Data: []byte("ok")}}
	server := NewServer(runtime, web)
	return server, runtime.Operations()[0].EventID
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) (httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var payload map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, recorder.Body.String())
		}
	}
	return *recorder, payload
}

func TestProtectedActionRequiresApprovalOverHTTP(t *testing.T) {
	server, eventID := approvalServer(t)
	recorder, _ := postJSON(t, server.Router(), "/api/control", map[string]any{
		"action": "stop-program", "event_id": string(eventID),
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestApprovalLifecycleOverHTTP(t *testing.T) {
	server, eventID := approvalServer(t)
	handler := server.Router()

	recorder, created := postJSON(t, handler, "/api/approvals", map[string]any{
		"event_id": string(eventID), "action": "hold-event", "initiator": "alice",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("approval id missing")
	}

	// Self-confirmation is rejected.
	recorder, _ = postJSON(t, handler, "/api/approvals/"+id+"/decision", map[string]any{
		"confirmer": "alice", "decision": "approve",
	})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("self-confirm status = %d", recorder.Code)
	}

	// Another operator confirms; the action executes.
	recorder, decided := postJSON(t, handler, "/api/approvals/"+id+"/decision", map[string]any{
		"confirmer": "bob", "decision": "approve",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("decision status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if decided["status"] != string(model.ApprovalExecuted) {
		t.Fatalf("approval status = %v", decided["status"])
	}
	if event, ok := server.runtime.Events.Get(eventID); !ok || event.Phase != model.PhaseHolding {
		t.Fatalf("event phase = %s", event.Phase)
	}

	// Listing exposes the terminal record with its timeline.
	request := httptest.NewRequest(http.MethodGet, "/api/approvals", nil)
	recorder2 := httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("list status = %d", recorder2.Code)
	}
	var list struct {
		Approvals []model.ApprovalRequest `json:"approvals"`
	}
	if err := json.Unmarshal(recorder2.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Approvals) != 1 || len(list.Approvals[0].Timeline) != 3 {
		t.Fatalf("approvals = %+v", list.Approvals)
	}
}

func TestApprovalRejectionAndDriftOverHTTP(t *testing.T) {
	server, eventID := approvalServer(t)
	handler := server.Router()

	recorder, created := postJSON(t, handler, "/api/approvals", map[string]any{
		"event_id": string(eventID), "action": "hold-event", "initiator": "alice",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	id := created["id"].(string)

	recorder, rejected := postJSON(t, handler, "/api/approvals/"+id+"/decision", map[string]any{
		"confirmer": "bob", "decision": "reject", "reason": "hold off",
	})
	if recorder.Code != http.StatusOK || rejected["status"] != string(model.ApprovalRejected) {
		t.Fatalf("reject status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	// A second approval goes stale while waiting: advance the program.
	_, second := postJSON(t, handler, "/api/approvals", map[string]any{
		"event_id": string(eventID), "action": "hold-event", "initiator": "alice",
	})
	secondID := second["id"].(string)
	control := service.ControlRequest{Action: "advance-program", EventID: string(eventID)}
	if _, err := server.runtime.ExecuteControl(httptest.NewRequest(http.MethodPost, "/", nil).Context(), control); err != nil {
		t.Fatal(err)
	}
	recorder, drifted := postJSON(t, handler, "/api/approvals/"+secondID+"/decision", map[string]any{
		"confirmer": "bob", "decision": "approve",
	})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("drift decision status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	approval, _ := drifted["approval"].(map[string]any)
	if approval["status"] != string(model.ApprovalInvalidated) {
		t.Fatalf("drifted approval = %v", approval)
	}
}
