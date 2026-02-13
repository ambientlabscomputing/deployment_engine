package runner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ambientlabscomputing/deployment_engine/internal/compiler"
)

// ExecutionResult holds the result of a deployment execution
type ExecutionResult struct {
	DeploymentID string                 `json:"deployment_id"`
	Status       string                 `json:"status"`
	Error        string                 `json:"error,omitempty"`
	Output       map[string]interface{} `json:"output,omitempty"`
}

// Runner executes compiled deployment graphs
type Runner struct {
	logger *slog.Logger
}

// NewRunner creates a new deployment runner
func NewRunner(logger *slog.Logger) (*Runner, error) {
	return &Runner{
		logger: logger,
	}, nil
}

// Execute runs a compiled deployment graph
func (r *Runner) Execute(ctx context.Context, graph *compiler.CompiledGraph) (*ExecutionResult, error) {
	r.logger.Info("executing deployment", "id", graph.DeploymentID, "services", len(graph.Services))

	// Validate graph before execution
	if err := graph.Validate(); err != nil {
		return &ExecutionResult{
			DeploymentID: graph.DeploymentID,
			Status:       "failed",
			Error:        err.Error(),
		}, fmt.Errorf("graph validation failed: %w", err)
	}

	result := &ExecutionResult{
		DeploymentID: graph.DeploymentID,
		Status:       "running",
		Output:       make(map[string]interface{}),
	}

	// Execute services
	for name, svc := range graph.Services {
		r.logger.Info("deploying service", "name", name, "image", svc.Image)
		result.Output[name] = map[string]interface{}{
			"image":  svc.Image,
			"status": "started",
		}
	}

	result.Status = "completed"
	r.logger.Info("deployment execution completed", "id", graph.DeploymentID)
	return result, nil
}

// Stop stops a running deployment
func (r *Runner) Stop(ctx context.Context, deploymentID string) error {
	r.logger.Info("stopping deployment", "id", deploymentID)
	return nil
}
