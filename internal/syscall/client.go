package syscall

import (
	"context"
	"fmt"
	"log/slog"

	pb "github.com/ambientlabscomputing/umc_sdk/proto/ua_kernel/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Client wraps the kernel syscall API for deployment engine operations
type Client struct {
	eventService    pb.EventServiceClient
	clusterService  pb.ClusterServiceClient
	execService     pb.ExecServiceClient
	identityService pb.IdentityServiceClient
	secretService   pb.SecretServiceClient
	providerService pb.ProviderServiceClient
	logger          *slog.Logger
}

// NewClient creates a new syscall client
func NewClient(conn *grpc.ClientConn, logger *slog.Logger) *Client {
	return &Client{
		eventService:    pb.NewEventServiceClient(conn),
		clusterService:  pb.NewClusterServiceClient(conn),
		execService:     pb.NewExecServiceClient(conn),
		identityService: pb.NewIdentityServiceClient(conn),
		secretService:   pb.NewSecretServiceClient(conn),
		providerService: pb.NewProviderServiceClient(conn),
		logger:          logger,
	}
}

// EmitDeploymentEvent publishes a deployment event to the kernel
func (c *Client) EmitDeploymentEvent(ctx context.Context, eventType, deploymentID string, payload map[string]interface{}) error {
	payloadStruct, err := structpb.NewStruct(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req := &pb.EmitEventRequest{
		EventType:  eventType,
		EntityKind: "deployment",
		EntityId:   deploymentID,
		Payload:    payloadStruct,
	}

	_, err = c.eventService.EmitEvent(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to emit event: %w", err)
	}

	c.logger.Info("deployment event emitted",
		"event_type", eventType,
		"deployment_id", deploymentID,
	)

	return nil
}

// SubscribeDeploymentEvents subscribes to deployment events from the kernel
func (c *Client) SubscribeDeploymentEvents(ctx context.Context) (pb.EventService_SubscribeLocalClient, error) {
	req := &pb.SubscribeLocalRequest{
		EventTypeFilter: []string{
			"deployment.created",
			"deployment.updated",
			"deployment.deleted",
			"deployment.completed",
			"deployment.failed",
		},
		BufferSizeHint: 100,
	}

	stream, err := c.eventService.SubscribeLocal(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to events: %w", err)
	}

	c.logger.Info("subscribed to deployment events")
	return stream, nil
}

// GetDeploymentState retrieves deployment state from cluster KV
func (c *Client) GetDeploymentState(ctx context.Context, deploymentID string) ([]byte, error) {
	key := fmt.Sprintf("/deployments/%s", deploymentID)
	req := &pb.GetKVRequest{Key: key}

	resp, err := c.clusterService.GetKV(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get kv: %w", err)
	}

	return resp.Value, nil
}

// SaveDeploymentState stores deployment state in cluster KV
func (c *Client) SaveDeploymentState(ctx context.Context, deploymentID string, state []byte) error {
	key := fmt.Sprintf("/deployments/%s", deploymentID)
	req := &pb.PutKVRequest{
		Key:   key,
		Value: state,
	}

	_, err := c.clusterService.PutKV(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to put kv: %w", err)
	}

	c.logger.Info("deployment state saved", "deployment_id", deploymentID)
	return nil
}

// RunProcess executes a command via the kernel ExecService
func (c *Client) RunProcess(ctx context.Context, command string, args []string, env map[string]string, workingDir string, timeoutSeconds uint32) (*pb.RunProcessResponse, error) {
	req := &pb.RunProcessRequest{
		Command:        command,
		Args:           args,
		Env:            env,
		WorkingDir:     workingDir,
		TimeoutSeconds: timeoutSeconds,
	}

	resp, err := c.execService.RunProcess(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to run process: %w", err)
	}

	c.logger.Info("process executed via kernel", "command", command, "exit_code", resp.ExitCode)
	return resp, nil
}

// SignPayload signs a payload using the node's identity key
func (c *Client) SignPayload(ctx context.Context, payload []byte) ([]byte, string, error) {
	req := &pb.SignPayloadRequest{Payload: payload}

	resp, err := c.identityService.SignPayload(ctx, req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to sign payload: %w", err)
	}

	return resp.Signature, resp.Algorithm, nil
}

// GetSecret retrieves a secret from the kernel SecretService
func (c *Client) GetSecret(ctx context.Context, key string) ([]byte, error) {
	req := &pb.GetSecretRequest{Key: key}

	resp, err := c.secretService.GetSecret(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret: %w", err)
	}

	return resp.Value, nil
}
