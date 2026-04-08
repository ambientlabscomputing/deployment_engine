package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ambientlabscomputing/deployment_engine/internal/compiler"
	mobycontainer "github.com/moby/moby/api/types/container"
	mobynetwork "github.com/moby/moby/api/types/network"
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

// Ping verifies the Docker daemon is reachable. Returns an error if it is not.
func (r *Runner) Ping(ctx context.Context) error {
	_, err := r.dockerClient.Ping(ctx, client.PingOptions{})
	return err
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
		containerName := fmt.Sprintf("%s-%s", graph.Slug, name)
		r.logger.Info("deploying service", "name", name, "container", containerName, "image", svc.Image)

		imageRef := svc.Image

		if svc.Build != nil {
			// Build from source context instead of pulling
			r.logger.Info("building image from source", "name", name, "archive_url", func() string {
				if svc.Source != nil {
					return svc.Source.ArchiveURL
				}
				return ""
			}())
			builtImage, buildErr := r.buildImage(ctx, graph.Slug, name, svc)
			if buildErr != nil {
				errMsg := fmt.Sprintf("failed to build image for %s: %v", name, buildErr)
				r.logger.Error("image build failed", "name", name, "error", buildErr)
				result.Output[name] = map[string]interface{}{
					"status": "failed",
					"error":  errMsg,
				}
				result.Status = "failed"
				result.Error = errMsg
				return result, errors.New(errMsg)
			}
			imageRef = builtImage
		} else {
			// Pull image if not present
			r.logger.Debug("pulling image", "image", imageRef)
			pullReader, err := r.dockerClient.ImagePull(ctx, imageRef, client.ImagePullOptions{})
			if err != nil {
				errMsg := fmt.Sprintf("failed to pull image %s: %v", imageRef, err)
				r.logger.Error("image pull failed", "image", imageRef, "error", err)
				result.Output[name] = map[string]interface{}{
					"image":  imageRef,
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
		}

		// Convert environment map to []string slice
		var env []string
		for k, v := range svc.Environment {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}

		// Parse port mappings (format: "hostPort:containerPort")
		exposedPorts := make(mobynetwork.PortSet)
		portBindings := make(mobynetwork.PortMap)
		for _, portSpec := range svc.Ports {
			parts := strings.SplitN(portSpec, ":", 2)
			if len(parts) == 2 {
				hostPort := parts[0]
				containerPort, err := mobynetwork.ParsePort(parts[1] + "/tcp")
				if err != nil {
					r.logger.Warn("invalid port spec, skipping", "port", portSpec, "error", err)
					continue
				}
				exposedPorts[containerPort] = struct{}{}
				portBindings[containerPort] = []mobynetwork.PortBinding{{HostPort: hostPort}}
			}
		}

		// Remove any existing container with the same name (idempotent redeploy)
		_, _ = r.dockerClient.ContainerRemove(ctx, containerName, client.ContainerRemoveOptions{Force: true})

		// Create container
		r.logger.Debug("creating container", "name", containerName)
		resp, err := r.dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: containerName,
			Config: &mobycontainer.Config{
				Image:        imageRef,
				Env:          env,
				ExposedPorts: exposedPorts,
				Labels: map[string]string{
					"underleaf.deployment_id": graph.DeploymentID,
					"underleaf.slug":          graph.Slug,
					"underleaf.service":       name,
				},
			},
			HostConfig: &mobycontainer.HostConfig{
				PortBindings: portBindings,
				RestartPolicy: mobycontainer.RestartPolicy{
					Name: mobycontainer.RestartPolicyUnlessStopped,
				},
			},
		})
		if err != nil {
			errMsg := fmt.Sprintf("failed to create container %s: %v", containerName, err)
			r.logger.Error("container creation failed", "name", containerName, "error", err)
			result.Output[name] = map[string]interface{}{
				"image":  imageRef,
				"status": "failed",
				"error":  errMsg,
			}
			result.Status = "failed"
			result.Error = errMsg
			return result, errors.New(errMsg)
		}

		// Start container
		r.logger.Debug("starting container", "name", containerName, "container_id", resp.ID)
		_, err = r.dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
		if err != nil {
			errMsg := fmt.Sprintf("failed to start container %s: %v", containerName, err)
			r.logger.Error("container start failed", "name", containerName, "container_id", resp.ID, "error", err)
			result.Output[name] = map[string]interface{}{
				"image":        imageRef,
				"container_id": resp.ID,
				"status":       "failed",
				"error":        errMsg,
			}
			result.Status = "failed"
			result.Error = errMsg
			return result, errors.New(errMsg)
		}

		result.Output[name] = map[string]interface{}{
			"image":        imageRef,
			"container_id": resp.ID,
			"status":       "running",
		}
		r.logger.Info("service deployed successfully", "name", name, "container_id", resp.ID)
	}

	result.Status = "completed"
	r.logger.Info("deployment execution completed", "id", graph.DeploymentID)
	return result, nil
}

// buildImage downloads the source archive and builds a Docker image from it.
// Returns the image tag that was built.
func (r *Runner) buildImage(ctx context.Context, slug, serviceName string, svc *compiler.ServiceNode) (string, error) {
	if svc.Source == nil || svc.Source.ArchiveURL == "" {
		return "", fmt.Errorf("service %s has a build config but no source archive URL", serviceName)
	}

	buildCfg := svc.Build
	contextPath := "."
	dockerfile := "Dockerfile"
	if buildCfg != nil {
		if buildCfg.Context != "" {
			contextPath = buildCfg.Context
		}
		if buildCfg.Dockerfile != "" {
			dockerfile = buildCfg.Dockerfile
		}
	}

	ref := svc.Source.Ref
	if ref == "" {
		ref = "local"
	}
	imageTag := fmt.Sprintf("%s/%s:%s", slug, serviceName, ref)

	r.logger.Info("downloading source archive",
		"url", svc.Source.ArchiveURL,
		"image", imageTag,
	)

	// Download the GitHub tarball
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Source.ArchiveURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download request: %w", err)
	}
	if svc.Source.Type == "github" || svc.Source.Type == "" {
		if svc.Source.Token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+svc.Source.Token)
		}
		httpReq.Header.Set("Accept", "application/vnd.github+json")
	}
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to download source archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("source archive download returned HTTP %d", resp.StatusCode)
	}

	// Extract to a temp directory, stripping the top-level GitHub-generated directory
	tmpDir, err := os.MkdirTemp("", "underleaf-build-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGzStripped(resp.Body, tmpDir); err != nil {
		return "", fmt.Errorf("failed to extract source archive: %w", err)
	}

	// Build context directory
	buildContextDir := path.Join(tmpDir, filepath.ToSlash(contextPath))
	dockerfilePath := path.Join(buildContextDir, filepath.ToSlash(dockerfile))
	if _, statErr := os.Stat(dockerfilePath); statErr != nil {
		return "", fmt.Errorf("Dockerfile not found at %s: %w", dockerfilePath, statErr)
	}

	// Create a tar stream for the Docker build context
	buildContextTar, err := tarDirectory(buildContextDir)
	if err != nil {
		return "", fmt.Errorf("failed to tar build context: %w", err)
	}
	defer buildContextTar.Close()

	// Assemble build args
	buildArgs := map[string]*string{}
	if buildCfg != nil {
		for k, v := range buildCfg.Args {
			val := v
			buildArgs[k] = &val
		}
	}

	r.logger.Info("building Docker image", "image", imageTag, "dockerfile", dockerfile)
	buildResp, err := r.dockerClient.ImageBuild(ctx, buildContextTar, client.ImageBuildOptions{
		Tags:       []string{imageTag},
		Dockerfile: dockerfile,
		BuildArgs:  buildArgs,
		Remove:     true,
		Labels:     map[string]string{"underleaf.slug": slug, "underleaf.service": serviceName},
	})
	if err != nil {
		return "", fmt.Errorf("docker image build failed: %w", err)
	}
	defer buildResp.Body.Close()

	// Stream build output to logger
	if _, copyErr := io.Copy(io.Discard, buildResp.Body); copyErr != nil {
		r.logger.Warn("error reading docker build output", "error", copyErr)
	}

	r.logger.Info("docker image built successfully", "image", imageTag)
	return imageTag, nil
}

// tarDirectory creates an in-memory tar archive of the given directory and
// returns a ReadCloser that streams the tar content.
func tarDirectory(dir string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	tw := tar.NewWriter(pw)

	go func() {
		err := filepath.Walk(dir, func(filePath string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(dir, filePath)
			if err != nil {
				return err
			}
			// Normalise to forward slashes for portability
			rel = filepath.ToSlash(rel)

			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
			f, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		tw.Close()
		pw.CloseWithError(err)
	}()

	return pr, nil
}

// extractTarGzStripped extracts a gzip-compressed tar into dst, stripping the// first (top-level) path component — which is how GitHub archives are structured.
func extractTarGzStripped(r io.Reader, dst string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a valid gzip stream: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading tar: %w", err)
		}

		// Strip first path component
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue // skip top-level directory entry
		}
		relPath := parts[1]

		target := path.Join(dst, relPath)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0750); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(path.Dir(target), 0750); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("create file %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write file %s: %w", target, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
		}
	}
	return nil
}

// Stop stops a running deployment by slug. Containers are matched by the
// "underleaf.slug" label (set on new deploys) or by name prefix "<slug>-"
// for containers created before labels were added.
func (r *Runner) Stop(ctx context.Context, slug string) error {
	r.logger.Info("stopping deployment", "slug", slug)

	listResult, err := r.dockerClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	namePrefix := slug + "-"
	var errs []error
	stopped := 0
	for _, ctr := range listResult.Items {
		// Match by label first (preferred).
		if ctr.Labels != nil {
			if ctr.Labels["underleaf.slug"] == slug {
				if stopErr := r.stopAndRemove(ctx, ctr.ID); stopErr != nil {
					errs = append(errs, stopErr)
				} else {
					stopped++
				}
				continue
			}
		}
		// Fallback: match by container name prefix.
		for _, n := range ctr.Names {
			if len(n) > 0 && n[0] == '/' {
				n = n[1:]
			}
			if strings.HasPrefix(n, namePrefix) {
				if stopErr := r.stopAndRemove(ctx, ctr.ID); stopErr != nil {
					errs = append(errs, stopErr)
				} else {
					stopped++
				}
				break
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("stopped %d containers with %d errors: %v", stopped, len(errs), errs)
	}

	r.logger.Info("deployment stopped", "slug", slug, "containers", stopped)
	return nil
}

func (r *Runner) stopAndRemove(ctx context.Context, containerID string) error {
	timeout := 10
	if _, err := r.dockerClient.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		r.logger.Warn("failed to stop container", "id", containerID, "error", err)
		return err
	}
	if _, err := r.dockerClient.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{}); err != nil {
		r.logger.Warn("failed to remove container", "id", containerID, "error", err)
		return err
	}
	r.logger.Info("container stopped and removed", "id", containerID)
	return nil
}

// ReconcileContainers inspects all containers with the "underleaf.deployment_id"
// label and restarts any that have exited. This is called at startup to recover
// from host reboots, daemon restarts, or OOM kills.
func (r *Runner) ReconcileContainers(ctx context.Context) (restarted int, errCount int) {
	r.logger.Info("reconciling deployment containers")

	listResult, err := r.dockerClient.ContainerList(ctx, client.ContainerListOptions{
		All: true, // include stopped containers
	})
	if err != nil {
		r.logger.Error("failed to list containers for reconciliation", "error", err)
		return 0, 1
	}

	for _, ctr := range listResult.Items {
		// Only manage containers that were created by the deployment engine.
		if ctr.Labels == nil || ctr.Labels["underleaf.deployment_id"] == "" {
			continue
		}

		if ctr.State == "running" {
			continue
		}

		// Container is not running (exited, created, dead, etc.) — restart it.
		containerName := ""
		if len(ctr.Names) > 0 {
			containerName = strings.TrimPrefix(ctr.Names[0], "/")
		}

		r.logger.Warn("found stopped underleaf container, restarting",
			"container_id", ctr.ID[:12],
			"name", containerName,
			"state", ctr.State,
			"deployment_id", ctr.Labels["underleaf.deployment_id"],
		)

		if _, startErr := r.dockerClient.ContainerStart(ctx, ctr.ID, client.ContainerStartOptions{}); startErr != nil {
			r.logger.Error("failed to restart container",
				"container_id", ctr.ID[:12],
				"name", containerName,
				"error", startErr,
			)
			errCount++
		} else {
			r.logger.Info("container restarted successfully",
				"container_id", ctr.ID[:12],
				"name", containerName,
				"deployment_id", ctr.Labels["underleaf.deployment_id"],
			)
			restarted++
		}
	}

	r.logger.Info("container reconciliation complete", "restarted", restarted, "errors", errCount)
	return restarted, errCount
}
