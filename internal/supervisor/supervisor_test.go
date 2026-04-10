package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateUMCName_Valid(t *testing.T) {
	valid := []string{"cron-engine", "helloworld", "my-umc-2", "a", "A1"}
	for _, name := range valid {
		if err := validateUMCName(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}
}

func TestValidateUMCName_Invalid(t *testing.T) {
	cases := []struct {
		name string
		desc string
	}{
		{"", "empty"},
		{"../etc", "path traversal"},
		{"foo/bar", "contains slash"},
		{"foo\\bar", "contains backslash"},
		{".hidden", "starts with dot"},
		{"-leading-dash", "starts with dash"},
		{"name with spaces", "contains spaces"},
		{"name;rm -rf /", "shell injection"},
		{string(make([]byte, 65)), "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if err := validateUMCName(tc.name); err == nil {
				t.Errorf("expected %q (%s) to be rejected", tc.name, tc.desc)
			}
		})
	}
}

func TestSanitizedEnv_AllowsOnlySafeVars(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/malicious.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/malicious.dylib")
	t.Setenv("PYTHONPATH", "/evil")
	t.Setenv("HOME", "/home/test")

	env := sanitizedEnv("/tmp/test.sock", "info")

	envMap := make(map[string]string)
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}

	if envMap["KERNEL_SOCKET"] != "/tmp/test.sock" {
		t.Error("KERNEL_SOCKET not set correctly")
	}
	if envMap["LOG_LEVEL"] != "info" {
		t.Error("LOG_LEVEL not set correctly")
	}
	if envMap["HOME"] != "/home/test" {
		t.Error("HOME should be allowed through")
	}

	for _, unsafe := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PYTHONPATH"} {
		if _, ok := envMap[unsafe]; ok {
			t.Errorf("%s should have been stripped from child env", unsafe)
		}
	}
}

func TestInstallUMC_RejectsHTTP(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	err := sup.InstallUMC("valid-umc", "http://evil.com/binary", "abc123")
	if err == nil {
		t.Fatal("expected error for HTTP URL")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("error should mention HTTPS, got: %s", err.Error())
	}
}

func TestInstallUMC_RejectsEmptyChecksum(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	err := sup.InstallUMC("valid-umc", "https://example.com/binary", "")
	if err == nil {
		t.Fatal("expected error for empty checksum")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error should mention checksum, got: %s", err.Error())
	}
}

func TestInstallUMC_RejectsPathTraversalName(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	err := sup.InstallUMC("../../../etc/passwd", "https://example.com/binary", "abc123")
	if err == nil {
		t.Fatal("expected error for path traversal name")
	}
}

func TestInstallUMC_RejectsInvalidURL(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	err := sup.InstallUMC("valid-umc", "://noscheme", "abc123")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestStartUMC_RejectsInvalidPort(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	for _, port := range []int{0, -1, 65536, 100000} {
		err := sup.StartUMC(nil, "valid-umc", port)
		if err == nil {
			t.Errorf("expected error for port %d", port)
		}
	}
}

func TestStartUMC_RejectsInvalidName(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	err := sup.StartUMC(nil, "../evil", 8080)
	if err == nil {
		t.Fatal("expected error for invalid UMC name")
	}
}

func TestFindExecutable_RejectsSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, ".underleaf", "bin")
	os.MkdirAll(binDir, 0755)

	target := filepath.Join(binDir, "test-umc-serve")
	os.WriteFile(target, []byte("#!/bin/sh"), 0755)

	symlink := filepath.Join(binDir, "evil-umc-serve")
	os.Symlink(target, symlink)

	t.Setenv("HOME", tmpDir)
	sup := NewSupervisor(SupervisorConfig{})

	if result := sup.findExecutable("test-umc"); result == "" {
		t.Error("expected to find real executable")
	}

	if result := sup.findExecutable("evil-umc"); result != "" {
		t.Errorf("expected symlink to be rejected, got: %s", result)
	}
}
