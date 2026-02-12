package runner
package runner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ambientlabscomputing/deployment_engine/internal/compiler"
	"github.com/moby/moby/client"
)

// Runner executes compiled deployment graphs




























































}	Output       map[string]interface{}	Error        string	Status       string	DeploymentID stringtype ExecutionResult struct {// ExecutionResult represents the result of execution}	return nil	// 4. Remove volumes	// 3. Remove networks	// 2. Remove services	// 1. Stop all services	// TODO: Implement deployment stop	r.logger.Info("stopping deployment", "deployment_id", deploymentID)func (r *Runner) Stop(ctx context.Context, deploymentID string) error {// Stop stops a running deployment}	return result, nil	// 5. Handle errors with cleanup	// 4. Track progress via context	// 3. Create and start services	// 2. Create volumes	// 1. Create networks	// TODO: Implement actual execution	}		Status:       "success",		DeploymentID: graph.DeploymentID,	result := &ExecutionResult{	)		"services", len(graph.Services),		"deployment_id", graph.DeploymentID,	r.logger.Info("executing deployment graph",func (r *Runner) Execute(ctx context.Context, graph *compiler.CompiledGraph) (*ExecutionResult, error) {// Execute runs a compiled graph}	}, nil		logger:       logger,		dockerClient: cli,	return &Runner{	}		return nil, fmt.Errorf("failed to create docker client: %w", err)	if err != nil {	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())func NewRunner(logger *slog.Logger) (*Runner, error) {// NewRunner creates a new runner}	logger       *slog.Logger	dockerClient *client.Clienttype Runner struct {