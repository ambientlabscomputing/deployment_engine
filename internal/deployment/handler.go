package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
)

// DeploymentSpec represents a full deployment specification
type DeploymentSpec struct {
	ID       string                 `json:"id"`
	Slug     string                 `json:"slug"`
	Version  string                 `json:"version"`
	Services map[string]ServiceSpec `json:"services"`
	Networks map[string]NetworkSpec `json:"networks,omitempty"`
	Volumes  map[string]VolumeSpec  `json:"volumes,omitempty"`
}

// ServiceSpec defines a service within a deployment
type ServiceSpec struct {
	Image       string            `json:"image"`
	Ports       []string          `json:"ports,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	Expose      *ExposeConfig     `json:"expose,omitempty"`
}

// ExposeConfig declares that a service should be publicly exposed via Hyphae tunnel.
type ExposeConfig struct {
	Port     int    `json:"port"`
	Hostname string `json:"hostname,omitempty"`
}

// NetworkSpec defines a network within a deployment
type NetworkSpec struct {
	Driver string `json:"driver"`
	Name   string `json:"name"`
}

// VolumeSpec defines a volume within a deployment
type VolumeSpec struct {
	Driver string            `json:"driver"`
	Name   string            `json:"name"`
	Config map[string]string `json:"config,omitempty"`
}

// Handler manages deployment lifecycle operations
type Handler struct {
	syscallClient *syscall.Client
	logger        *slog.Logger
}

// NewHandler creates a new deployment handler
func NewHandler(syscallClient *syscall.Client, logger *slog.Logger) *Handler {
	return &Handler{
		syscallClient: syscallClient,
		logger:        logger,
	}
}

// Deploy executes a deployment specification
func (h *Handler) Deploy(ctx context.Context, spec *DeploymentSpec) error {
	h.logger.Info("deploying", "id", spec.ID, "slug", spec.Slug, "version", spec.Version)

	// Persist deployment state
	stateBytes, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("failed to marshal deployment spec: %w", err)
	}

	if err := h.syscallClient.SaveDeploymentState(ctx, spec.ID, stateBytes); err != nil {
		h.logger.Error("failed to save deployment state", "id", spec.ID, "error", err)
		return fmt.Errorf("failed to save state: %w", err)
	}

	// Emit deployment created event
	payload := map[string]interface{}{
		"slug":     spec.Slug,
		"version":  spec.Version,
		"services": len(spec.Services),
	}

	if err := h.syscallClient.EmitDeploymentEvent(ctx, "deployment.created", spec.ID, payload); err != nil {
		h.logger.Error("failed to emit deployment event", "id", spec.ID, "error", err)
		return fmt.Errorf("failed to emit event: %w", err)
	}

	// Announce exposures for services that declare public access
	for svcName, svc := range spec.Services {
		if svc.Expose == nil {
			continue
		}
		if err := h.EmitServiceExposure(ctx, spec.ID, svcName, svc.Expose.Port, ""); err != nil {
			h.logger.Error("failed to emit service exposure", "id", spec.ID, "service", svcName, "error", err)
			// Non-fatal: log and continue
		}
	}

	h.logger.Info("deployment created", "id", spec.ID)
	return nil
}

// Delete removes a deployment
func (h *Handler) Delete(ctx context.Context, deploymentID string) error {
	h.logger.Info("deleting deployment", "id", deploymentID)

	payload := map[string]interface{}{
		"deployment_id": deploymentID,
	}

	if err := h.syscallClient.EmitDeploymentEvent(ctx, "deployment.deleted", deploymentID, payload); err != nil {
		h.logger.Error("failed to emit delete event", "id", deploymentID, "error", err)
		return fmt.Errorf("failed to emit event: %w", err)
	}

	h.logger.Info("deployment deleted", "id", deploymentID)
	return nil
}

// GetStatus retrieves the current status of a deployment
func (h *Handler) GetStatus(ctx context.Context, deploymentID string) (map[string]interface{}, error) {
	stateBytes, err := h.syscallClient.GetDeploymentState(ctx, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment state: %w", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal deployment state: %w", err)
	}

	return state, nil
}

// EmitServiceExposure emits an event indicating a service is exposed and ready
// This is called after a service container is created and verified to be healthy
func (h *Handler) EmitServiceExposure(ctx context.Context, deploymentID, serviceName string, targetPort int, containerID string) error {
	h.logger.Info("emitting service exposure event", "deployment_id", deploymentID, "service", serviceName, "port", targetPort)

	payload := map[string]interface{}{
		"deployment_id": deploymentID,
		"service_name":  serviceName,
		"port":          targetPort,
		"container_id":  containerID,
	}

	if err := h.syscallClient.EmitDeploymentEvent(ctx, "deployment.service.exposed", deploymentID, payload); err != nil {
		h.logger.Error("failed to emit service exposure event", "deployment_id", deploymentID, "service", serviceName, "error", err)
		return fmt.Errorf("failed to emit service exposure event: %w", err)
	}

	h.logger.Info("service exposure event emitted", "deployment_id", deploymentID, "service", serviceName)
	return nil
}
