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

		// Create container
		r.logger.Debug("creating container", "name", name)
		resp, err := r.dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: name,
			Config: &mobycontainer.Config{
				Image: imageRef,
				Env:   env,
			},
		})
		if err != nil {
			errMsg := fmt.Sprintf("failed to create container %s: %v", name, err)
			r.logger.Error("container creation failed", "name", name, "error", err)
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
		r.logger.Debug("starting container", "name", name, "container_id", resp.ID)
		_, err = r.dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
		if err != nil {
			errMsg := fmt.Sprintf("failed to start container %s: %v", name, err)
			r.logger.Error("container start failed", "name", name, "container_id", resp.ID, "error", err)
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

	imageTag := fmt.Sprintf("%s/%s:%s", slug, serviceName, svc.Source.Ref)

	r.logger.Info("downloading source archive",
		"url", svc.Source.ArchiveURL,
		"image", imageTag,
	)

	// Download the GitHub tarball
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Source.ArchiveURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download request: %w", err)
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
