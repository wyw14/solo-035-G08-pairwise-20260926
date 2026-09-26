package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wyw14/solo-035/internal/service"
)

func (server *Server) control(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input service.ControlRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid control payload")
		return
	}
	result, err := server.runtime.ExecuteControl(request.Context(), input)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, service.ErrApprovalRequired) {
			// Critical director actions must be proposed and confirmed by a
			// second operator before they can run.
			status = http.StatusForbidden
		}
		writeJSON(writer, status, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) detail(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, server.runtime.Detail())
}

func (server *Server) exportCheckpoint(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, server.runtime.ExportCheckpoint())
}

func (server *Server) restoreCheckpoint(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var checkpoint service.ControlCheckpoint
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid checkpoint payload")
		return
	}
	report, err := server.runtime.RestoreCheckpoint(checkpoint)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, report)
}
