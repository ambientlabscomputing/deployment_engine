package compiler

import (
	"strings"
	"testing"

	"github.com/ambientlabscomputing/deployment_engine/internal/deployment"
)

// TestSecretPattern_Matching verifies the secret pattern regex correctly identifies secret placeholders
func TestSecretPattern_Matching(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
		desc     string
	}{
		{"${secret:db/password}", true, "basic secret reference"},
		{"${secret:api-key}", true, "with dash"},
		{"${secret:nested/deep/key}", true, "nested path"},
		{"${secret:key_with_underscore}", true, "with underscore"},
		{"${secret:key.with.dots}", true, "with dots"},
		{"prefix-${secret:key}-suffix", true, "embedded in string"},
		{"${ secret:key}", false, "space after ${"},
		{"${secret: key}", false, "space after colon"},
		{"${notasecret:key}", false, "wrong prefix"},
		{"secret:key", false, "missing ${}"},
		{"", false, "empty string"},
		{"no-secrets-here", false, "plain string"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			matches := secretPattern.MatchString(tt.input)
			if matches != tt.expected {
				t.Errorf("Pattern match for %q: expected %v, got %v", tt.input, tt.expected, matches)
			}
		})
	}
}

// TestSecretPattern_Extraction verifies the regex correctly extracts secret keys
func TestSecretPattern_Extraction(t *testing.T) {
	tests := []struct {
		input       string
		expectedKey string
		desc        string
	}{
		{"${secret:db/password}", "db/password", "simple path"},
		{"${secret:api-key}", "api-key", "with dash"},
		{"${secret:nested/deep/key}", "nested/deep/key", "nested path"},
		{"${secret:_underscore_key}", "_underscore_key", "underscores"},
		{"${secret:key.with.dots}", "key.with.dots", "dots"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			submatch := secretPattern.FindStringSubmatch(tt.input)
			if len(submatch) < 2 {
				t.Fatalf("Expected to extract secret key from %q", tt.input)
			}
			if submatch[1] != tt.expectedKey {
				t.Errorf("Expected key %q, got %q", tt.expectedKey, submatch[1])
			}
		})
	}
}

// TestCompile_ValidatesDeploymentSpec verifies input validation
func TestCompile_ValidatesDeploymentSpec(t *testing.T) {
	compiler := &Compiler{
		syscallClient: nil,
	}

	tests := []struct {
		name    string
		spec    *deployment.DeploymentSpec
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil spec",
			spec:    nil,
			wantErr: true,
			errMsg:  "deployment spec is nil",
		},
		{
			name: "missing ID",
			spec: &deployment.DeploymentSpec{
				Version: "1.0",
				Services: map[string]deployment.ServiceSpec{
					"web": {Image: "nginx"},
				},
			},
			wantErr: true,
			errMsg:  "deployment spec missing ID",
		},
		{
			name: "valid spec",
			spec: &deployment.DeploymentSpec{
				ID:      "test-123",
				Version: "1.0",
				Slug:    "test-app",
				Services: map[string]deployment.ServiceSpec{
					"web": {
						Image: "nginx:latest",
						Environment: map[string]string{
							"PORT": "8080",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compiler.Compile(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Errorf("Compile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Expected error to contain %q, got %q", tt.errMsg, err.Error())
				}
			}
		})
	}
}

// TestCompiledGraph_Validate verifies graph validation catches errors
func TestCompiledGraph_Validate(t *testing.T) {
	tests := []struct {
		name    string
		graph   *CompiledGraph
		wantErr bool
		errMsg  string
	}{
		{
			name: "missing deployment ID",
			graph: &CompiledGraph{
				Services: map[string]*ServiceNode{
					"web": {Name: "web", Image: "nginx"},
				},
			},
			wantErr: true,
			errMsg:  "missing deployment ID",
		},
		{
			name: "no services",
			graph: &CompiledGraph{
				DeploymentID: "test-123",
				Services:     map[string]*ServiceNode{},
			},
			wantErr: true,
			errMsg:  "has no services",
		},
		{
			name: "service missing image",
			graph: &CompiledGraph{
				DeploymentID: "test-123",
				Services: map[string]*ServiceNode{
					"web": {Name: "web"},
				},
			},
			wantErr: true,
			errMsg:  "has no image",
		},
		{
			name: "service references unknown network",
			graph: &CompiledGraph{
				DeploymentID: "test-123",
				Services: map[string]*ServiceNode{
					"web": {
						Name:     "web",
						Image:    "nginx",
						Networks: []string{"unknown-net"},
					},
				},
				Networks: map[string]*NetworkNode{},
			},
			wantErr: true,
			errMsg:  "references unknown network",
		},
		{
			name: "valid graph",
			graph: &CompiledGraph{
				DeploymentID: "test-123",
				Services: map[string]*ServiceNode{
					"web": {
						Name:     "web",
						Image:    "nginx",
						Networks: []string{"app-net"},
					},
				},
				Networks: map[string]*NetworkNode{
					"app-net": {Name: "app-net", Driver: "bridge"},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.graph.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Expected error to contain %q, got %q", tt.errMsg, err.Error())
				}
			}
		})
	}
}

// TestCompile_PreservesServiceMetadata verifies all service data is correctly transformed
func TestCompile_PreservesServiceMetadata(t *testing.T) {
	compiler := &Compiler{syscallClient: nil}

	spec := &deployment.DeploymentSpec{
		ID:      "test-deploy-1",
		Version: "1.0.0",
		Slug:    "test-app",
		Services: map[string]deployment.ServiceSpec{
			"web": {
				Image: "nginx:latest",
				Ports: []string{"80:8080", "443:8443"},
				Environment: map[string]string{
					"ENV":  "production",
					"PORT": "8080",
				},
				Volumes:  []string{"/data:/app/data", "/logs:/app/logs"},
				Networks: []string{"frontend", "backend"},
			},
		},
		Networks: map[string]deployment.NetworkSpec{
			"frontend": {Name: "frontend", Driver: "bridge"},
			"backend":  {Name: "backend", Driver: "overlay"},
		},
		Volumes: map[string]deployment.VolumeSpec{
			"data": {Name: "data-vol", Driver: "local"},
		},
	}

	graph, err := compiler.Compile(spec)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}

	// Verify deployment metadata is preserved
	if graph.DeploymentID != "test-deploy-1" {
		t.Errorf("Expected DeploymentID=test-deploy-1, got %s", graph.DeploymentID)
	}
	if graph.Version != "1.0.0" {
		t.Errorf("Expected Version=1.0.0, got %s", graph.Version)
	}
	if graph.Slug != "test-app" {
		t.Errorf("Expected Slug=test-app, got %s", graph.Slug)
	}

	// Verify service compilation preserves all data
	webService := graph.Services["web"]
	if webService == nil {
		t.Fatal("Web service not in compiled graph")
	}

	if webService.Image != "nginx:latest" {
		t.Errorf("Expected image=nginx:latest, got %s", webService.Image)
	}

	if len(webService.Ports) != 2 {
		t.Errorf("Expected 2 ports, got %d", len(webService.Ports))
	}

	if len(webService.Environment) != 2 {
		t.Errorf("Expected 2 environment variables, got %d", len(webService.Environment))
	}

	if webService.Environment["ENV"] != "production" {
		t.Errorf("Expected ENV=production, got %s", webService.Environment["ENV"])
	}

	if len(webService.VolumeMounts) != 2 {
		t.Errorf("Expected 2 volume mounts, got %d", len(webService.VolumeMounts))
	}

	if len(webService.Networks) != 2 {
		t.Errorf("Expected 2 networks, got %d", len(webService.Networks))
	}

	// Verify network compilation
	if len(graph.Networks) != 2 {
		t.Errorf("Expected 2 networks, got %d", len(graph.Networks))
	}

	frontend := graph.Networks["frontend"]
	if frontend == nil {
		t.Fatal("Frontend network not in compiled graph")
	}
	if frontend.Driver != "bridge" {
		t.Errorf("Expected frontend driver=bridge, got %s", frontend.Driver)
	}

	// Verify volume compilation
	if len(graph.Volumes) != 1 {
		t.Errorf("Expected 1 volume, got %d", len(graph.Volumes))
	}

	dataVol := graph.Volumes["data"]
	if dataVol == nil {
		t.Fatal("Data volume not in compiled graph")
	}
	if dataVol.Name != "data-vol" {
		t.Errorf("Expected volume name=data-vol, got %s", dataVol.Name)
	}
}
