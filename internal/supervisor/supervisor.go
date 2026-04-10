package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// validUMCName matches alphanumeric names with optional hyphens (no path separators).
var validUMCName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\-]*$`)

// validateUMCName rejects names that could cause path traversal or shell injection.
func validateUMCName(name string) error {
	if name == "" {
		return fmt.Errorf("UMC name is empty")
	}
	if !validUMCName.MatchString(name) {
		return fmt.Errorf("invalid UMC name %q: must be alphanumeric with optional hyphens", name)
	}
	if len(name) > 64 {
		return fmt.Errorf("UMC name too long: max 64 characters")
	}
	return nil
}

// sanitizedEnv returns a clean environment for child processes, carrying over
// only safe variables and adding the required KERNEL_SOCKET and LOG_LEVEL.
func sanitizedEnv(kernelSocket, logLevel string) []string {
	// Allowlist of environment variables safe to inherit.
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LANG": true,
		"TERM": true, "TMPDIR": true, "TZ": true,
		"XDG_RUNTIME_DIR": true, "DOCKER_HOST": true,
	}
	var env []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if allowed[key] {
			env = append(env, e)
		}
	}
	env = append(env, "KERNEL_SOCKET="+kernelSocket, "LOG_LEVEL="+logLevel)
	return env
}

// RestartPolicy defines how a UMC should be restarted on failure
type RestartPolicy string

const (
	RestartPolicyAlways    RestartPolicy = "always"
	RestartPolicyOnFailure RestartPolicy = "on-failure"
	RestartPolicyNever     RestartPolicy = "never"
)

// UMCProcess represents a managed Underleaf Micro-Component process
type UMCProcess struct {
	ID             string
	Name           string
	Command        *exec.Cmd
	PID            int
	Port           int
	Running        bool
	RestartPolicy  RestartPolicy
	RestartCount   int
	LastRestartAt  time.Time
	BackoffSeconds int
	mu             sync.Mutex
	stopChan       chan struct{}
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
	if err := validateUMCName(umcName); err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d: must be 1-65535", port)
	}

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

	// Prepare command with sanitized environment
	cmd := exec.CommandContext(ctx, exePath)
	cmd.Env = sanitizedEnv(s.config.KernelSocket, s.config.LogLevel)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start UMC %s: %w", umcName, err)
	}

	proc := &UMCProcess{
		ID:             umcName,
		Name:           umcName,
		Command:        cmd,
		PID:            cmd.Process.Pid,
		Port:           port,
		Running:        true,
		RestartPolicy:  RestartPolicyAlways, // Default to always restart
		BackoffSeconds: 1,                   // Initial backoff
		stopChan:       make(chan struct{}),
	}

	s.processes[umcName] = proc
	s.logger.Info("UMC started", "name", umcName, "pid", cmd.Process.Pid)

	// Write PID file for cron-engine so parent UA can track it for cleanup
	if umcName == "cron-engine" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			pidDir := filepath.Join(homeDir, ".underleaf", "run")
			_ = os.MkdirAll(pidDir, 0700)
			pidFile := filepath.Join(pidDir, "cron_engine.pid")
			if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0600); err != nil {
				s.logger.Warn("failed to write cron engine PID file", "error", err)
			}
		}
	}

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

// RestartUMC stops and restarts a managed UMC
func (s *Supervisor) RestartUMC(ctx context.Context, umcName string) error {
	s.mu.RLock()
	proc, exists := s.processes[umcName]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("UMC not found: %s", umcName)
	}

	if !proc.Running {
		return fmt.Errorf("UMC is not running: %s", umcName)
	}

	s.logger.Info("Restarting UMC", "name", umcName)

	// Stop the UMC
	if err := s.StopUMC(ctx, umcName); err != nil {
		return fmt.Errorf("failed to stop UMC: %w", err)
	}

	// Small delay to ensure clean port release
	time.Sleep(500 * time.Millisecond)

	// Start it again with the same configuration
	return s.StartUMC(ctx, umcName, proc.Port)
}

// SetRestartPolicy updates the restart policy for a managed UMC
func (s *Supervisor) SetRestartPolicy(umcName string, policy RestartPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	proc, exists := s.processes[umcName]
	if !exists {
		return fmt.Errorf("UMC not found: %s", umcName)
	}

	proc.RestartPolicy = policy
	s.logger.Info("Updated restart policy", "name", umcName, "policy", policy)
	return nil
}

// InstallUMC downloads and installs a UMC binary from a URL.
// Requires HTTPS and a non-empty SHA-256 checksum.
func (s *Supervisor) InstallUMC(umcName, artifactURL, checksum string) error {
	// ── Input validation ───────────────────────────────────────────────
	if err := validateUMCName(umcName); err != nil {
		return err
	}

	parsed, err := url.Parse(artifactURL)
	if err != nil {
		return fmt.Errorf("invalid artifact URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("artifact URL must use HTTPS (got %q)", parsed.Scheme)
	}

	if checksum == "" {
		return fmt.Errorf("checksum is required for binary installation")
	}

	// ── Prepare paths ──────────────────────────────────────────────────
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	binDir := filepath.Join(homeDir, ".underleaf", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin directory: %w", err)
	}

	exeName := umcName + "-serve"
	targetPath := filepath.Join(binDir, exeName)
	tempPath := targetPath + ".tmp"

	s.logger.Info("Downloading UMC binary", "name", umcName, "url", artifactURL)

	// ── Download ───────────────────────────────────────────────────────
	resp, err := http.Get(artifactURL) //nolint:gosec // URL scheme validated above
	if err != nil {
		return fmt.Errorf("failed to download binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status: %s", resp.Status)
	}

	// ── Write to temp file while computing SHA-256 ─────────────────────
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tempPath)

	var h hash.Hash = sha256.New()
	writer := io.MultiWriter(tempFile, h)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		tempFile.Close()
		return fmt.Errorf("failed to write binary: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// ── Verify checksum (always) ────────────────────────────────────────
	actualChecksum := hex.EncodeToString(h.Sum(nil))
	if actualChecksum != checksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", checksum, actualChecksum)
	}
	s.logger.Info("Checksum verified", "name", umcName, "checksum", checksum)

	// ── Make executable and atomically install ─────────────────────────
	if err := os.Chmod(tempPath, 0755); err != nil {
		return fmt.Errorf("failed to make binary executable: %w", err)
	}

	if err := os.Rename(tempPath, targetPath); err != nil {
		if os.IsPermission(err) || os.IsExist(err) {
			return fmt.Errorf("failed to install binary: %w", err)
		}
		// Cross-device, do a copy
		if err := s.copyFile(tempPath, targetPath); err != nil {
			return fmt.Errorf("failed to install binary: %w", err)
		}
	}

	s.logger.Info("UMC binary installed successfully", "name", umcName, "path", targetPath)
	return nil
}

// copyFile copies a file from src to dst (for cross-device installs)
func (s *Supervisor) copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return err
	}

	// Preserve permissions
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	return os.Chmod(dst, srcInfo.Mode())
}

// findExecutable locates the UMC binary. Uses Lstat to avoid following
// symlinks that could redirect execution to an attacker-controlled path.
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
		// Lstat does NOT follow symlinks — prevents symlink redirect attacks.
		info, err := os.Lstat(p)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			s.logger.Warn("skipping symlink in executable search", "path", p)
			continue
		}
		return p
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

// monitorUMC monitors a UMC process and handles restarts
func (s *Supervisor) monitorUMC(umcName string, proc *UMCProcess) {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		// Wait for process to finish
		exitErr := proc.Command.Wait()
		exitCode := 0
		if exitErr != nil {
			if exitError, ok := exitErr.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			}
			s.logger.Warn("UMC process exited", "name", umcName, "exit_code", exitCode, "error", exitErr)
		} else {
			s.logger.Info("UMC process exited cleanly", "name", umcName)
		}

		proc.mu.Lock()
		proc.Running = false
		restartPolicy := proc.RestartPolicy
		proc.mu.Unlock()

		// Don't restart if supervisor is shutting down
		select {
		case <-s.stopChan:
			return
		default:
		}

		// Determine if we should restart
		shouldRestart := false
		switch restartPolicy {
		case RestartPolicyAlways:
			shouldRestart = true
		case RestartPolicyOnFailure:
			shouldRestart = (exitCode != 0)
		case RestartPolicyNever:
			shouldRestart = false
		}

		if !shouldRestart {
			s.logger.Info("UMC not restarting per policy", "name", umcName, "policy", restartPolicy)
			return
		}

		// Increment restart count and calculate backoff
		proc.mu.Lock()
		proc.RestartCount++
		restartCount := proc.RestartCount

		// Exponential backoff: 1s, 2s, 4s, 8s, 16s, 32s, 60s (max)
		backoff := time.Duration(proc.BackoffSeconds) * time.Second
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		} else {
			proc.BackoffSeconds *= 2
			if proc.BackoffSeconds > 60 {
				proc.BackoffSeconds = 60
			}
		}

		// Reset backoff if healthy for 5 minutes
		if !proc.LastRestartAt.IsZero() && time.Since(proc.LastRestartAt) > 5*time.Minute {
			proc.BackoffSeconds = 1
			backoff = time.Second
		}

		proc.LastRestartAt = time.Now()
		proc.mu.Unlock()

		s.logger.Info("UMC restarting after backoff",
			"name", umcName,
			"restart_count", restartCount,
			"backoff_seconds", backoff.Seconds())

		// Wait for backoff period
		select {
		case <-time.After(backoff):
			// Continue to restart
		case <-s.stopChan:
			return
		}

		// Find executable again (in case it was updated)
		exePath := s.findExecutable(umcName)
		if exePath == "" {
			s.logger.Error("UMC executable not found for restart", "name", umcName)
			return
		}

		// Restart the process with sanitized environment
		cmd := exec.Command(exePath)
		cmd.Env = sanitizedEnv(s.config.KernelSocket, s.config.LogLevel)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			s.logger.Error("failed to restart UMC", "name", umcName, "error", err)
			return
		}

		proc.mu.Lock()
		proc.Command = cmd
		proc.PID = cmd.Process.Pid
		proc.Running = true
		proc.mu.Unlock()

		s.logger.Info("UMC restarted successfully", "name", umcName, "pid", cmd.Process.Pid)
	}
}
