package compiler

import (
	"fmt"

	"github.com/ambientlabscomputing/deployment_engine/internal/deployment"
)

// Compiler transforms deployment specs into executable graphs
type Compiler struct{}

// NewCompiler creates a new compiler instance
func NewCompiler() *Compiler {
	return &Compiler{}
}

// CompiledGraph represents a compiled deployment graph ready for execution
type CompiledGraph struct {
	DeploymentID string                     `json:"deployment_id"`
	Version      string                     `json:"version"`
	Slug         string                     `json:"slug"`
	Services     map[string]*ServiceNode    `json:"services"`
	Networks     map[string]*NetworkNode    `json:"networks"`
	Volumes      map[string]*VolumeNode     `json:"volumes"`
}

// ServiceNode represents a service in the compiled graph
type ServiceNode struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	Environment  map[string]string `json:"environment,omitempty"`
	Ports        []string          `json:"ports,omitempty"`
	VolumeMounts []string          `json:"volume_mounts,omitempty"`
	Networks     []string          `json:"networks,omitempty"`
	DependsOn    []string          `json:"depends_on,omitempty"`
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
		graph.Services[name] = &ServiceNode{
			Name:         name,
			Image:        svc.Image,
			Environment:  svc.Environment,
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
		if svc.Image == "" {
			return fmt.Errorf("service %s has no image", name)
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
