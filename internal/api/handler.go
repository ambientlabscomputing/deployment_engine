package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ambientlabscomputing/deployment_engine/internal/supervisor"
	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
)

// Handler handles HTTP API requests for deployment operations
type Handler struct {
	syscallClient *syscall.Client
	supervisor    *supervisor.Supervisor
	logger        *slog.Logger
}

// NewHandler creates a new API handler
func NewHandler(syscallClient *syscall.Client, logger *slog.Logger) *Handler {
	return &Handler{
		syscallClient: syscallClient,
		logger:        logger,
	}
}

// SetSupervisor sets the supervisor for this handler
func (h *Handler) SetSupervisor(sup *supervisor.Supervisor) {
	h.supervisor = sup
}

// ServeHTTP routes HTTP requests to appropriate handlers
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Route supervisor requests
	if strings.HasPrefix(r.URL.Path, "/supervisor") {
		if h.supervisor == nil {
			http.Error(w, "supervisor not available", http.StatusServiceUnavailable)
			return
		}
		h.handleSupervisor(w, r)
		return
	}

	switch r.URL.Path {
	case "/health":
		h.handleHealth(w, r)
	case "/deployments":
		h.handleDeployments(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleHealth returns health status
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

// handleSupervisor routes supervisor requests
func (h *Handler) handleSupervisor(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/supervisor/umc":
		h.handleSupervisorUMC(w, r)
	case "/supervisor/umcs":
		h.handleSupervisorUMCs(w, r)
	case "/supervisor/umc/status":
		h.handleSupervisorUMCStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleSupervisorUMC handles POST requests to start UMCs
func (h *Handler) handleSupervisorUMC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if req.Port == 0 {
		http.Error(w, "port is required", http.StatusBadRequest)
		return
	}

	// Start the UMC
	if err := h.supervisor.StartUMC(r.Context(), req.Name, req.Port); err != nil {
		h.logger.Warn("failed to start UMC", "name", req.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "started",
		"name":   req.Name,
	})
}

// handleSupervisorUMCs handles GET requests to list UMCs
func (h *Handler) handleSupervisorUMCs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	umcs := h.supervisor.ListUMCs()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"umcs": umcs,
	})
}

// handleSupervisorUMCStatus handles GET requests for UMC status
func (h *Handler) handleSupervisorUMCStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	umcName := r.URL.Query().Get("name")
	if umcName == "" {
		http.Error(w, "name query parameter is required", http.StatusBadRequest)
		return
	}

	status, err := h.supervisor.StatusUMC(umcName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleDeployments routes deployment requests
func (h *Handler) handleDeployments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleCreateDeployment(w, r)
	case http.MethodGet:
		h.handleGetDeployment(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCreateDeployment creates a new deployment
func (h *Handler) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string                 `json:"id"`
		Spec map[string]interface{} `json:"spec"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Emit deployment event
	if err := h.syscallClient.EmitDeploymentEvent(
		context.Background(),
		"deployment.created",
		req.ID,
		req.Spec,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id": req.ID,
	})
}

// handleGetDeployment retrieves deployment state
func (h *Handler) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := r.URL.Query().Get("id")
	if deploymentID == "" {
		http.Error(w, "missing deployment id", http.StatusBadRequest)
		return
	}

	state, err := h.syscallClient.GetDeploymentState(context.Background(), deploymentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":    deploymentID,
		"state": string(state),
	})
}
