package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ambientlabscomputing/deployment_engine/internal/api"
	"github.com/ambientlabscomputing/deployment_engine/internal/supervisor"
	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
	"github.com/ambientlabscomputing/umc_sdk/lifecycle"
	"github.com/ambientlabscomputing/umc_sdk/logging"
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

	httpServer := &http.Server{
		Addr:         ":8080",
		Handler:      apiHandler,
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

	// Auto-start cron engine if configured
	if os.Getenv("AUTO_START_CRON_ENGINE") == "true" {
		if err := c.supervisor.StartUMC(ctx, "cron-engine", 8081); err != nil {
			c.logger.Warn("failed to auto-start cron engine", "error", err)
			// Don't fail startup if cron engine fails
		} else {
			c.logger.Info("cron engine auto-started via supervisor")
		}
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

// dialKernelSyscallServer attempts to connect to the UA kernel syscall server
func dialKernelSyscallServer(timeout time.Duration) (*grpc.ClientConn, error) {
	socketPath := os.Getenv("KERNEL_SOCKET")
	if socketPath == "" {
		socketPath = "/tmp/ua_kernel.sock"
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dialer := net.Dialer{}
	conn, err := grpc.DialContext(
		ctx,
		"unix:"+socketPath,
		grpc.WithInsecure(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial kernel syscall server at %s: %w", socketPath, err)
	}

	return conn, nil
}
