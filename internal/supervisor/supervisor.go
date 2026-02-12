package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// UMCProcess represents a managed Underleaf Micro-Component process
type UMCProcess struct {
	ID       string
	Name     string
	Command  *exec.Cmd
	PID      int
	Port     int
	Running  bool
	mu       sync.Mutex
	stopChan chan struct{}
}

// SupervisorConfig holds configuration for the supervisor
type SupervisorConfig struct {
	KernelSocket     string
	BinDir           string
	LogLevel         string
	HealthTimeout    time.Duration
	GracefulShutdown time.Duration
}

// Supervisor manages the lifecycle of sibling UMCs
type Supervisor struct {
	config    SupervisorConfig
	processes map[string]*UMCProcess
	mu        sync.RWMutex
	logger    *slog.Logger
	stopChan  chan struct{}
	wg        sync.WaitGroup
}

// NewSupervisor creates a new supervisor instance
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 5 * time.Second
	}
	if cfg.GracefulShutdown == 0 {
		cfg.GracefulShutdown = 5 * time.Second
	}

	return &Supervisor{
		config:    cfg,
		processes: make(map[string]*UMCProcess),
		logger:    slog.Default(),
		stopChan:  make(chan struct{}),
	}
}

// StartUMC starts a managed UMC by name (e.g., "cron-engine")
func (s *Supervisor) StartUMC(ctx context.Context, umcName string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if already running
	if proc, exists := s.processes[umcName]; exists && proc.Running {
		return fmt.Errorf("UMC %s is already running (PID: %d)", umcName, proc.PID)
	}

	// Find executable
	exePath := s.findExecutable(umcName)
	if exePath == "" {
		return fmt.Errorf("UMC executable not found for %s", umcName)
	}

	// Prepare command
	cmd := exec.CommandContext(ctx, exePath)
	cmd.Env = append(os.Environ(),
		"KERNEL_SOCKET="+s.config.KernelSocket,
		"LOG_LEVEL="+s.config.LogLevel,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start UMC %s: %w", umcName, err)
	}

	proc := &UMCProcess{
		ID:       umcName,
		Name:     umcName,
		Command:  cmd,
		PID:      cmd.Process.Pid,
		Port:     port,
		Running:  true,
		stopChan: make(chan struct{}),
	}

	s.processes[umcName] = proc
	s.logger.Info("UMC started", "name", umcName, "pid", cmd.Process.Pid)

	// Monitor process health
	s.wg.Add(1)
	go s.monitorUMC(umcName, proc)

	return nil
}

// StopUMC gracefully stops a managed UMC
func (s *Supervisor) StopUMC(ctx context.Context, umcName string) error {
	s.mu.Lock()
	proc, exists := s.processes[umcName]
	s.mu.Unlock()

	if !exists {
		return fmt.Errorf("UMC %s not found", umcName)
	}

	if !proc.Running {
		return fmt.Errorf("UMC %s is not running", umcName)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	// Send SIGTERM for graceful shutdown
	if err := proc.Command.Process.Signal(os.Interrupt); err != nil {
		s.logger.Warn("failed to send SIGTERM to UMC", "name", umcName, "error", err)
	}

	// Wait with timeout
	timeout := time.NewTimer(s.config.GracefulShutdown)
	defer timeout.Stop()

	done := make(chan error, 1)
	go func() {
		done <- proc.Command.Wait()
	}()

	select {
	case <-timeout.C:
		s.logger.Warn("UMC graceful shutdown timeout, killing process", "name", umcName)
		_ = proc.Command.Process.Kill()
		_ = proc.Command.Wait()
	case err := <-done:
		if err != nil {
			s.logger.Info("UMC stopped", "name", umcName, "error", err)
		} else {
			s.logger.Info("UMC stopped", "name", umcName)
		}
	}

	proc.Running = false
	return nil
}

// StatusUMC returns the status of a managed UMC
func (s *Supervisor) StatusUMC(umcName string) (map[string]interface{}, error) {
	s.mu.RLock()
	proc, exists := s.processes[umcName]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("UMC %s not found", umcName)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	status := map[string]interface{}{
		"name":    proc.Name,
		"running": proc.Running,
		"pid":     proc.PID,
		"port":    proc.Port,
	}

	// Check health if running
	if proc.Running {
		healthy := s.checkHealth(proc)
		status["healthy"] = healthy
	}

	return status, nil
}

// ListUMCs returns all managed UMCs
func (s *Supervisor) ListUMCs() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []map[string]interface{}
	for _, proc := range s.processes {
		status, _ := s.StatusUMC(proc.Name)
		result = append(result, status)
	}
	return result
}

// Shutdown gracefully stops all managed UMCs
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	umcNames := make([]string, 0, len(s.processes))
	for name := range s.processes {
		umcNames = append(umcNames, name)
	}
	s.mu.Unlock()

	var errors []error
	for _, name := range umcNames {
		if err := s.StopUMC(ctx, name); err != nil {
			errors = append(errors, err)
		}
	}

	close(s.stopChan)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// Wait for monitor goroutines with timeout
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("supervisor shutdown timeout")
	}

	if len(errors) > 0 {
		return fmt.Errorf("supervisor shutdown had errors: %v", errors)
	}

	return nil
}

// findExecutable locates the UMC binary
func (s *Supervisor) findExecutable(umcName string) string {
	homeDir, _ := os.UserHomeDir()
	exeName := umcName + "-serve"

	// Check these paths in order:
	// 1. ~/.underleaf/bin/{name}-serve
	// 2. /usr/local/bin/{name}-serve
	// 3. Relative to deployment_engine binary location

	paths := []string{
		filepath.Join(homeDir, ".underleaf", "bin", exeName),
		filepath.Join("/usr/local/bin", exeName),
		filepath.Join(filepath.Dir(os.Args[0]), "..", umcName, "serve"),
	}

	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}

	return ""
}

// checkHealth checks if a UMC is responding to health checks
func (s *Supervisor) checkHealth(proc *UMCProcess) bool {
	if !proc.Running {
		return false
	}

	url := fmt.Sprintf("http://localhost:%d/health", proc.Port)
	client := &http.Client{Timeout: s.config.HealthTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// monitorUMC monitors a UMC process
func (s *Supervisor) monitorUMC(umcName string, proc *UMCProcess) {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		// Wait for process to finish
		if err := proc.Command.Wait(); err != nil {
			s.logger.Warn("UMC process exited", "name", umcName, "error", err)
		}

		proc.mu.Lock()
		proc.Running = false
		proc.mu.Unlock()

		// Don't restart if supervisor is shutting down
		select {
		case <-s.stopChan:
			return
		default:
		}

		// Log that the process has stopped
		s.logger.Info("UMC process monitor stopping", "name", umcName)
		break
	}
}
