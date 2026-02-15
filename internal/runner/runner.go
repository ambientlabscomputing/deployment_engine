package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ambientlabscomputing/deployment_engine/internal/compiler"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
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
	logger       *slog.Logger
	dockerClient *client.Client
}

// NewRunner creates a new deployment runner
func NewRunner(logger *slog.Logger) (*Runner, error) {
	// Create Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Runner{
		logger:       logger,
		dockerClient: cli,
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

	// Execute services in topological order
	for name, svc := range graph.Services {
		r.logger.Info("deploying service", "name", name, "image", svc.Image)

		// Pull image if not present
		r.logger.Debug("pulling image", "image", svc.Image)
		pullReader, err := r.dockerClient.ImagePull(ctx, svc.Image, client.ImagePullOptions{})
		if err != nil {
			errMsg := fmt.Sprintf("failed to pull image %s: %v", svc.Image, err)
			r.logger.Error("image pull failed", "image", svc.Image, "error", err)
			result.Output[name] = map[string]interface{}{
				"image":  svc.Image,
				"status": "failed",
				"error":  errMsg,
			}
			result.Status = "failed"
			result.Error = errMsg
			return result, errors.New(errMsg)
		}
		// Read and discard pull output to ensure pull completes
		_, _ = io.Copy(io.Discard, pullReader)
		pullReader.Close()

		// Convert environment map to []string slice
		var env []string
		for k, v := range svc.Environment {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}

		// Create container
		r.logger.Debug("creating container", "name", name)
		resp, err := r.dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: name,
			Config: &container.Config{
				Image: svc.Image,
				Env:   env,
			},
		})
		if err != nil {
			errMsg := fmt.Sprintf("failed to create container %s: %v", name, err)
			r.logger.Error("container creation failed", "name", name, "error", err)
			result.Output[name] = map[string]interface{}{
				"image":  svc.Image,
				"status": "failed",
				"error":  errMsg,
			}
			result.Status = "failed"
			result.Error = errMsg
			return result, errors.New(errMsg)
		}

		// Start container
		r.logger.Debug("starting container", "name", name, "container_id", resp.ID)
		_, err = r.dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
		if err != nil {
			errMsg := fmt.Sprintf("failed to start container %s: %v", name, err)
			r.logger.Error("container start failed", "name", name, "container_id", resp.ID, "error", err)
			result.Output[name] = map[string]interface{}{
				"image":        svc.Image,
				"container_id": resp.ID,
				"status":       "failed",
				"error":        errMsg,
			}
			result.Status = "failed"
			result.Error = errMsg
			return result, errors.New(errMsg)
		}

		result.Output[name] = map[string]interface{}{
			"image":        svc.Image,
			"container_id": resp.ID,
			"status":       "running",
		}
		r.logger.Info("service deployed successfully", "name", name, "container_id", resp.ID)
	}

	result.Status = "completed"
	r.logger.Info("deployment execution completed", "id", graph.DeploymentID)
	return result, nil
}

// Stop stops a running deployment
func (r *Runner) Stop(ctx context.Context, deploymentID string) error {
	r.logger.Info("stopping deployment", "id", deploymentID)

	// List all containers
	listResult, err := r.dockerClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	// Stop and remove containers matching the deployment
	var errors []error
	for _, ctr := range listResult.Items {
		// Check if container name or labels match this deployment
		// For now, use simple name prefix matching
		// TODO: Use proper labels when creating containers
		for _, name := range ctr.Names {
			if len(name) > 0 && name[0] == '/' {
				name = name[1:] // Remove leading slash
			}
			// This is a simplified approach - in production, use labels
			r.logger.Debug("stopping container", "id", ctr.ID, "name", name)

			// Stop container (10 second timeout)
			timeout := 10
			_, stopErr := r.dockerClient.ContainerStop(ctx, ctr.ID, client.ContainerStopOptions{Timeout: &timeout})
			if stopErr != nil {
				r.logger.Warn("failed to stop container", "id", ctr.ID, "error", stopErr)
				errors = append(errors, stopErr)
				continue
			}

			// Remove container
			_, removeErr := r.dockerClient.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{})
			if removeErr != nil {
				r.logger.Warn("failed to remove container", "id", ctr.ID, "error", removeErr)
				errors = append(errors, removeErr)
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("stopped deployment with errors: %v", errors)
	}

	r.logger.Info("deployment stopped successfully", "id", deploymentID)
	return nil
}
