package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-035/internal/model"
	"github.com/wyw14/solo-035/internal/service"
)

func (server *Server) listApprovals(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": server.runtime.ListApprovals()})
}

func (server *Server) createApproval(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input service.ApprovalSubmission
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid approval payload")
		return
	}
	approval, err := server.runtime.SubmitApproval(input)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, approval)
}

func (server *Server) getApproval(writer http.ResponseWriter, request *http.Request) {
	id := model.ApprovalID(chi.URLParam(request, "id"))
	approval, err := server.runtime.GetApproval(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, approval)
}

func (server *Server) decideApproval(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input service.ApprovalDecision
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid decision payload")
		return
	}
	approval, err := server.runtime.DecideApproval(request.Context(), model.ApprovalID(chi.URLParam(request, "id")), input)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, model.ErrApprovalNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(writer, status, map[string]any{"error": err.Error(), "approval": approval})
		return
	}
	// The dual-control handshake completed; a failed action is reported as
	// 422 so callers can distinguish it from a successful execution while the
	// terminal approval record stays in the body.
	status := http.StatusOK
	if approval.Status == model.ApprovalExecutionFailed {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(writer, status, approval)
}
