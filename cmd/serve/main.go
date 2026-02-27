package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ambientlabscomputing/deployment_engine/internal/api"
	"github.com/ambientlabscomputing/deployment_engine/internal/manifest"
	"github.com/ambientlabscomputing/deployment_engine/internal/supervisor"
	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
	"github.com/ambientlabscomputing/umc_sdk/lifecycle"
	"github.com/ambientlabscomputing/umc_sdk/logging"
	"github.com/ambientlabscomputing/umc_sdk/middleware"
	"github.com/ambientlabscomputing/umc_sdk/transport"
	"google.golang.org/grpc"
)

func main() {
	// Initialize logging
	logger, err := logging.Setup(logging.Config{
		Level:  "info",
		Format: "json",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup logging: %v\n", err)
		os.Exit(1)
	}

	// Connect to UA kernel syscall server
	conn, err := dialKernelSyscallServer(5 * time.Second)
	if err != nil {
		logger.Error("failed to connect to kernel syscall server", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Create syscall client
	syscallClient := syscall.NewClient(conn, logger)
	logger.Info("kernel syscall client connected")

	// Create supervisor for managing sibling UMCs
	kernelSocket := os.Getenv("KERNEL_SOCKET")
	if kernelSocket == "" {
		kernelSocket = "/tmp/ua_kernel.sock"
	}

	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		KernelSocket:     kernelSocket,
		LogLevel:         os.Getenv("LOG_LEVEL"),
		HealthTimeout:    5 * time.Second,
		GracefulShutdown: 5 * time.Second,
	})

	// Create API server
	apiHandler := api.NewHandler(syscallClient, logger)
	apiHandler.SetSupervisor(sup)

	// Wrap with trace middleware
	wrappedHandler := middleware.TraceIDMiddleware(apiHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "10081"
	}

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	// Create component launcher
	launcher := lifecycle.NewLauncher(logger)

	// Add supervisor component (manages other UMCs)
	launcher.Add(&SupervisorComponent{
		supervisor: sup,
		logger:     logger,
	})

	// Add HTTP server component
	launcher.Add(&HTTPServerComponent{
		server: httpServer,
		logger: logger,
	})

	// Add event subscriber component (subscribes to deployment events)
	launcher.Add(&EventSubscriberComponent{
		syscallClient: syscallClient,
		logger:        logger,
	})

	// Create runtime for signal handling
	runtime := lifecycle.NewRuntime(launcher)

	// Run runtime (blocks until signal)
	if err := runtime.Run(); err != nil {
		logger.Error("runtime error", "error", err)
		os.Exit(1)
	}
}

// SupervisorComponent implements lifecycle.Component for the supervisor
type SupervisorComponent struct {
	supervisor *supervisor.Supervisor
	logger     *slog.Logger
}

func (c *SupervisorComponent) Name() string {
	return "supervisor"
}

func (c *SupervisorComponent) Start(ctx context.Context) error {
	c.logger.Info("supervisor component starting")

	// Load UMC manifest (default to ./umcs.yaml or from UMCS_MANIFEST env var)
	manifestPath := os.Getenv("UMCS_MANIFEST")
	if manifestPath == "" {
		// Try current directory, then executable directory
		cwd, _ := os.Getwd()
		manifestPath = filepath.Join(cwd, "umcs.yaml")
		if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
			exePath, _ := os.Executable()
			manifestPath = filepath.Join(filepath.Dir(exePath), "umcs.yaml")
		}
	}

	// Load manifest if it exists
	if _, err := os.Stat(manifestPath); err == nil {
		c.logger.Info("loading UMC manifest", "path", manifestPath)
		m, err := manifest.Parse(manifestPath)
		if err != nil {
			c.logger.Warn("failed to parse UMC manifest, skipping auto-start", "error", err)
		} else {
			// Start all UMCs defined in the manifest
			for _, umc := range m.UMCs {
				c.logger.Info("auto-starting UMC from manifest", "name", umc.Name, "port", umc.Port, "restart_policy", umc.RestartPolicy)
				if err := c.supervisor.StartUMC(ctx, umc.Name, umc.Port); err != nil {
					c.logger.Warn("failed to auto-start UMC", "name", umc.Name, "error", err)
					// Continue starting other UMCs even if one fails
				}
			}
		}
	} else {
		c.logger.Info("no UMC manifest found, skipping auto-start", "expected_path", manifestPath)
	}

	return nil
}

func (c *SupervisorComponent) Stop(ctx context.Context) error {
	c.logger.Info("supervisor component stopping")
	return c.supervisor.Shutdown(ctx)
}

// HTTPServerComponent implements lifecycle.Component for HTTP server
type HTTPServerComponent struct {
	server *http.Server
	logger *slog.Logger
}

func (c *HTTPServerComponent) Name() string {
	return "http-server"
}

func (c *HTTPServerComponent) Start(ctx context.Context) error {
	c.logger.Info("starting http server", "addr", c.server.Addr)
	go func() {
		if err := c.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			c.logger.Error("http server error", "error", err)
		}
	}()
	return nil
}

func (c *HTTPServerComponent) Stop(ctx context.Context) error {
	return c.server.Shutdown(ctx)
}

// EventSubscriberComponent implements lifecycle.Component for event subscription
type EventSubscriberComponent struct {
	syscallClient *syscall.Client
	logger        *slog.Logger
	stopChan      chan struct{}
}

func (c *EventSubscriberComponent) Name() string {
	return "event-subscriber"
}

func (c *EventSubscriberComponent) Start(ctx context.Context) error {
	c.logger.Info("event subscriber component starting")
	c.stopChan = make(chan struct{})

	// Start event subscription in background goroutine
	go func() {
		for {
			select {
			case <-c.stopChan:
				c.logger.Info("event subscriber stopping")
				return
			default:
				// Subscribe to deployment events
				stream, err := c.syscallClient.SubscribeDeploymentEvents(ctx)
				if err != nil {
					c.logger.Error("failed to subscribe to events", "error", err)
					time.Sleep(5 * time.Second) // Wait before retry
					continue
				}

				// Read events from stream
				for {
					select {
					case <-c.stopChan:
						c.logger.Info("event subscriber stopping")
						return
					default:
						event, err := stream.Recv()
						if err != nil {
							c.logger.Warn("event stream error, reconnecting", "error", err)
							time.Sleep(2 * time.Second)
							break // Break inner loop to reconnect
						}

						// Log received event
						c.logger.Info("received deployment event",
							"event_id", event.EventId,
							"event_type", event.EventType,
							"entity_kind", event.EntityKind,
							"entity_id", event.EntityId,
							"emitted_at", event.EmittedAt.AsTime())

						// TODO: Process event (e.g., trigger deployment actions)
						// For now, just logging is sufficient for wiring
					}
				}
			}
		}
	}()

	return nil
}

func (c *EventSubscriberComponent) Stop(ctx context.Context) error {
	c.logger.Info("event subscriber component stopping")
	if c.stopChan != nil {
		close(c.stopChan)
	}
	return nil
}

// dialKernelSyscallServer connects to the UA kernel syscall server via Unix domain socket
func dialKernelSyscallServer(timeout time.Duration) (*grpc.ClientConn, error) {
	socketPath := os.Getenv("KERNEL_SOCKET")
	if socketPath == "" {
		socketPath = "/tmp/ua_kernel.sock"
	}

	conn, err := transport.UDSDialer(socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to dial kernel syscall server at %s: %w", socketPath, err)
	}

	return conn, nil
}
