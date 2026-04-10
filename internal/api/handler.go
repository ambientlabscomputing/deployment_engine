package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ambientlabscomputing/deployment_engine/internal/supervisor"
	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
)

// maxRequestBodyBytes limits request body size to prevent OOM from oversized payloads.
const maxRequestBodyBytes = 1 << 20 // 1 MB

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
	case "/supervisor/umc/restart":
		h.handleSupervisorUMCRestart(w, r)
	case "/supervisor/umc/install":
		h.handleSupervisorUMCInstall(w, r)
	case "/supervisor/umc/policy":
		h.handleSupervisorUMCPolicy(w, r)
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

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
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

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Emit deployment event
	if err := h.syscallClient.EmitDeploymentEvent(
		r.Context(),
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

	state, err := h.syscallClient.GetDeploymentState(r.Context(), deploymentID)
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

// handleSupervisorUMCRestart handles POST requests to restart a UMC
func (h *Handler) handleSupervisorUMCRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if err := h.supervisor.RestartUMC(r.Context(), req.Name); err != nil {
		h.logger.Warn("failed to restart UMC", "name", req.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "restarted",
		"name":   req.Name,
	})
}

// handleSupervisorUMCInstall handles POST requests to install a UMC binary
func (h *Handler) handleSupervisorUMCInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name        string `json:"name"`
		ArtifactURL string `json:"artifact_url"`
		Checksum    string `json:"checksum"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if req.ArtifactURL == "" {
		http.Error(w, "artifact_url is required", http.StatusBadRequest)
		return
	}

	if req.Checksum == "" {
		http.Error(w, "checksum is required", http.StatusBadRequest)
		return
	}

	if err := h.supervisor.InstallUMC(req.Name, req.ArtifactURL, req.Checksum); err != nil {
		h.logger.Warn("failed to install UMC", "name", req.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "installed",
		"name":   req.Name,
	})
}

// handleSupervisorUMCPolicy handles PUT requests to update a UMC's restart policy
func (h *Handler) handleSupervisorUMCPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name   string `json:"name"`
		Policy string `json:"policy"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if req.Policy == "" {
		http.Error(w, "policy is required", http.StatusBadRequest)
		return
	}

	// Validate policy value
	var policy supervisor.RestartPolicy
	switch req.Policy {
	case "always":
		policy = supervisor.RestartPolicyAlways
	case "on-failure":
		policy = supervisor.RestartPolicyOnFailure
	case "never":
		policy = supervisor.RestartPolicyNever
	default:
		http.Error(w, "invalid policy: must be 'always', 'on-failure', or 'never'", http.StatusBadRequest)
		return
	}

	if err := h.supervisor.SetRestartPolicy(req.Name, policy); err != nil {
		h.logger.Warn("failed to set restart policy", "name", req.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "updated",
		"name":   req.Name,
		"policy": req.Policy,
	})
}
