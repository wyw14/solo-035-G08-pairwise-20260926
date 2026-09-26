package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-035/internal/service"
)

type Server struct {
	runtime     *service.Runtime
	coordinator *service.Coordinator
	web         fs.FS
}

func NewServer(runtime *service.Runtime, web fs.FS) *Server {
	return &Server{runtime: runtime, coordinator: service.NewCoordinator(runtime), web: web}
}

func (server *Server) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", server.health)
	router.Route("/api", func(api chi.Router) {
		api.Get("/operations", server.operations)
		api.Post("/operations", server.createOperation)
		api.Get("/equipment", server.equipment)
		api.Get("/interlocks", server.interlocks)
		api.Get("/incidents", server.incidents)
		api.Get("/snapshot", server.snapshot)
		api.Get("/detail", server.detail)
		api.Post("/control", server.control)
		api.Get("/approvals", server.listApprovals)
		api.Post("/approvals", server.createApproval)
		api.Get("/approvals/{id}", server.getApproval)
		api.Post("/approvals/{id}/decision", server.decideApproval)
		api.Get("/checkpoint", server.exportCheckpoint)
		api.Post("/checkpoint", server.restoreCheckpoint)
	})
	router.Get("/", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/operations.html", http.StatusTemporaryRedirect)
	})
	router.Handle("/*", http.FileServer(http.FS(server.web)))
	return router
}
