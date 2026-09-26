package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-035/internal/approval"
	"github.com/wyw14/solo-035/internal/service"
)

// operatorHeader carries the acting director's identity. Two-person approval
// requires the confirmer to differ from the initiator.
const operatorHeader = "X-Operator"

func (server *Server) createApproval(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input service.ApprovalProposalRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid approval payload")
		return
	}
	if strings.TrimSpace(input.Initiator) == "" {
		input.Initiator = request.Header.Get(operatorHeader)
	}
	ticket, err := server.runtime.ProposeControl(input)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, ticket)
}

func (server *Server) listApprovals(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": server.runtime.ListApprovals()})
}

func (server *Server) getApproval(writer http.ResponseWriter, request *http.Request) {
	ticket, ok := server.runtime.GetApproval(chi.URLParam(request, "id"))
	if !ok {
		writeError(writer, http.StatusNotFound, "approval ticket not found")
		return
	}
	writeJSON(writer, http.StatusOK, ticket)
}

func (server *Server) decideApproval(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input service.ApprovalDecisionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid decision payload")
		return
	}
	if strings.TrimSpace(input.Approver) == "" {
		input.Approver = request.Header.Get(operatorHeader)
	}
	outcome, err := server.runtime.DecideControl(request.Context(), chi.URLParam(request, "id"), input)
	if err != nil {
		status := http.StatusConflict
		switch {
		case errors.Is(err, approval.ErrTicketNotFound):
			status = http.StatusNotFound
		case errors.Is(err, approval.ErrSameOperator):
			status = http.StatusUnprocessableEntity
		}
		writeJSON(writer, status, map[string]any{"error": err.Error(), "ticket": outcome.Ticket})
		return
	}
	writeJSON(writer, http.StatusOK, outcome)
}
