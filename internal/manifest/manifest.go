package manifest

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/ambientlabscomputing/deployment_engine/internal/supervisor"
)

// UMCManifest defines the configuration for managed UMCs
type UMCManifest struct {
	UMCs []UMCConfig `yaml:"umcs"`
}

// UMCConfig holds the configuration for a single UMC
type UMCConfig struct {
	Name          string `yaml:"name"`
	Port          int    `yaml:"port"`
	RestartPolicy string `yaml:"restart_policy"`
}

// Parse reads and parses a UMC manifest file
func Parse(filePath string) (*UMCManifest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest file: %w", err)
	}

	var manifest UMCManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest YAML: %w", err)
	}

	// Validate the manifest
	if err := manifest.Validate(); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// Validate checks that the manifest is well-formed
func (m *UMCManifest) Validate() error {
	if len(m.UMCs) == 0 {
		return fmt.Errorf("manifest must define at least one UMC")
	}

	seen := make(map[string]bool)
	for i, umc := range m.UMCs {
		if umc.Name == "" {
			return fmt.Errorf("UMC at index %d missing name", i)
		}

		if seen[umc.Name] {
			return fmt.Errorf("duplicate UMC name: %s", umc.Name)
		}
		seen[umc.Name] = true

		if umc.Port <= 0 || umc.Port > 65535 {
			return fmt.Errorf("UMC %s has invalid port: %d", umc.Name, umc.Port)
		}

		// Validate restart policy
		switch umc.RestartPolicy {
		case "always", "on-failure", "never":
			// Valid
		case "":
			// Default to "always" if not specified
			m.UMCs[i].RestartPolicy = "always"
		default:
			return fmt.Errorf("UMC %s has invalid restart_policy: %s (must be 'always', 'on-failure', or 'never')", umc.Name, umc.RestartPolicy)
		}
	}

	return nil
}

// GetRestartPolicy converts string policy to supervisor.RestartPolicy
func (c *UMCConfig) GetRestartPolicy() supervisor.RestartPolicy {
	switch c.RestartPolicy {
	case "on-failure":
		return supervisor.RestartPolicyOnFailure
	case "never":
		return supervisor.RestartPolicyNever
	default:
		return supervisor.RestartPolicyAlways
	}
}
