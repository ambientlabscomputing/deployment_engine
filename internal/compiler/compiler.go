package compiler

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ambientlabscomputing/deployment_engine/internal/deployment"
	"github.com/ambientlabscomputing/deployment_engine/internal/syscall"
)

// Compiler transforms deployment specs into executable graphs
type Compiler struct {
	syscallClient *syscall.Client
}

// NewCompiler creates a new compiler instance
func NewCompiler(syscallClient *syscall.Client) *Compiler {
	return &Compiler{
		syscallClient: syscallClient,
	}
}

// CompiledGraph represents a compiled deployment graph ready for execution
type CompiledGraph struct {
	DeploymentID string                  `json:"deployment_id"`
	Version      string                  `json:"version"`
	Slug         string                  `json:"slug"`
	Services     map[string]*ServiceNode `json:"services"`
	Networks     map[string]*NetworkNode `json:"networks"`
	Volumes      map[string]*VolumeNode  `json:"volumes"`
}

// ServiceNode represents a service in the compiled graph
type ServiceNode struct {
	Name         string                  `json:"name"`
	Image        string                  `json:"image,omitempty"`
	Build        *deployment.BuildConfig `json:"build,omitempty"`
	Source       *deployment.SourceRef   `json:"source,omitempty"`
	Environment  map[string]string       `json:"environment,omitempty"`
	Ports        []string                `json:"ports,omitempty"`
	VolumeMounts []string                `json:"volume_mounts,omitempty"`
	Networks     []string                `json:"networks,omitempty"`
	DependsOn    []string                `json:"depends_on,omitempty"`
}

// NetworkNode represents a network in the compiled graph
type NetworkNode struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// VolumeNode represents a volume in the compiled graph
type VolumeNode struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// Compile transforms a deployment spec into a compiled graph
func (c *Compiler) Compile(spec *deployment.DeploymentSpec) (*CompiledGraph, error) {
	if spec == nil {
		return nil, fmt.Errorf("deployment spec is nil")
	}

	if spec.ID == "" {
		return nil, fmt.Errorf("deployment spec missing ID")
	}

	graph := &CompiledGraph{
		DeploymentID: spec.ID,
		Version:      spec.Version,
		Slug:         spec.Slug,
		Services:     make(map[string]*ServiceNode),
		Networks:     make(map[string]*NetworkNode),
		Volumes:      make(map[string]*VolumeNode),
	}

	// Compile services
	for name, svc := range spec.Services {
		// Resolve secrets in environment variables
		resolvedEnv, err := c.resolveSecrets(context.Background(), svc.Environment)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve secrets for service %s: %w", name, err)
		}

		graph.Services[name] = &ServiceNode{
			Name:         name,
			Image:        svc.Image,
			Build:        svc.Build,
			Source:       spec.Source,
			Environment:  resolvedEnv,
			Ports:        svc.Ports,
			VolumeMounts: svc.Volumes,
			Networks:     svc.Networks,
		}
	}

	// Compile networks
	for name, net := range spec.Networks {
		graph.Networks[name] = &NetworkNode{
			Name:   net.Name,
			Driver: net.Driver,
		}
	}

	// Compile volumes
	for name, vol := range spec.Volumes {
		graph.Volumes[name] = &VolumeNode{
			Name:   vol.Name,
			Driver: vol.Driver,
		}
	}

	return graph, nil
}

// Validate checks a compiled graph for consistency errors
func (g *CompiledGraph) Validate() error {
	if g.DeploymentID == "" {
		return fmt.Errorf("compiled graph missing deployment ID")
	}

	if len(g.Services) == 0 {
		return fmt.Errorf("compiled graph has no services")
	}

	// Validate service references
	for name, svc := range g.Services {
		if svc.Image == "" && svc.Build == nil {
			return fmt.Errorf("service %s has no image and no build config", name)
		}

		// Validate network references
		for _, netName := range svc.Networks {
			if _, ok := g.Networks[netName]; !ok {
				return fmt.Errorf("service %s references unknown network %s", name, netName)
			}
		}
	}

	return nil
}

var secretPattern = regexp.MustCompile(`\$\{secret:([a-zA-Z0-9_\-\.\/]+)\}`)

// resolveSecrets replaces ${secret:key} placeholders with actual secret values.
// Returns an error if any secret cannot be resolved — partial deployments with
// missing secrets are a security/reliability risk.
func (c *Compiler) resolveSecrets(ctx context.Context, env map[string]string) (map[string]string, error) {
	if c.syscallClient == nil {
		// No syscall client available, return environment as-is
		return env, nil
	}

	resolved := make(map[string]string, len(env))
	for key, value := range env {
		// Check if value contains secret reference
		if !strings.Contains(value, "${secret:") {
			resolved[key] = value
			continue
		}

		// Replace all secret references in the value
		var resolveErr error
		resolvedValue := secretPattern.ReplaceAllStringFunc(value, func(match string) string {
			if resolveErr != nil {
				return match // already failed, skip remaining
			}
			// Extract secret key from ${secret:key}
			submatch := secretPattern.FindStringSubmatch(match)
			if len(submatch) < 2 {
				return match // Keep original if pattern doesn't match
			}
			secretKey := submatch[1]

			// Fetch secret from kernel
			secretValue, err := c.syscallClient.GetSecret(ctx, secretKey)
			if err != nil {
				resolveErr = fmt.Errorf("failed to resolve secret %q for env var %q: %w", secretKey, key, err)
				return match
			}

			return string(secretValue)
		})

		if resolveErr != nil {
			return nil, resolveErr
		}

		resolved[key] = resolvedValue
	}

	return resolved, nil
}
