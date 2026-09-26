package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

type createOperationRequest struct {
	Name string `json:"name"`
}

func (server *Server) createOperation(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var input createOperationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid operation payload")
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(writer, http.StatusBadRequest, "operation name is required")
		return
	}
	event, err := server.coordinator.CreateEvent(input.Name)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, event)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message any) {
	writeJSON(writer, status, map[string]any{"error": message, "status": status})
}
