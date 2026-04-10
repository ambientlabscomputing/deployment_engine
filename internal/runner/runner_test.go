package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name       string
	body       string
	isDir      bool
	linkTarget string
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, e := range entries {
		switch {
		case e.isDir:
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeDir, Mode: 0755})
		case e.linkTarget != "":
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeSymlink, Linkname: e.linkTarget})
		default:
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Size: int64(len(e.body)), Mode: 0644})
			tw.Write([]byte(e.body))
		}
	}

	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func TestExtractTarGzStripped_Normal(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "repo-abc123/", isDir: true},
		{name: "repo-abc123/hello.txt", body: "hello world"},
		{name: "repo-abc123/sub/", isDir: true},
		{name: "repo-abc123/sub/nested.txt", body: "nested"},
	})

	dst := t.TempDir()
	if err := extractTarGzStripped(bytes.NewReader(data), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dst, "hello.txt"))
	if err != nil {
		t.Fatalf("expected hello.txt: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("unexpected content: %s", content)
	}

	content, err = os.ReadFile(filepath.Join(dst, "sub", "nested.txt"))
	if err != nil {
		t.Fatalf("expected sub/nested.txt: %v", err)
	}
	if string(content) != "nested" {
		t.Errorf("unexpected content: %s", content)
	}
}

func TestExtractTarGzStripped_ZipSlipPath(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "repo-abc123/", isDir: true},
		{name: "repo-abc123/../../etc/passwd", body: "pwned"},
	})

	dst := t.TempDir()
	err := extractTarGzStripped(bytes.NewReader(data), dst)
	if err == nil {
		t.Fatal("expected error for Zip Slip path traversal")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("expected 'path traversal' in error, got: %s", err.Error())
	}
}

func TestExtractTarGzStripped_SymlinkEscape(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "repo-abc123/", isDir: true},
		{name: "repo-abc123/evil-link", linkTarget: "/etc/passwd"},
	})

	dst := t.TempDir()
	err := extractTarGzStripped(bytes.NewReader(data), dst)
	if err == nil {
		t.Fatal("expected error for symlink escaping root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected 'symlink' in error, got: %s", err.Error())
	}
}

func TestExtractTarGzStripped_SymlinkInsideOK(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "repo-abc123/", isDir: true},
		{name: "repo-abc123/target.txt", body: "real file"},
		{name: "repo-abc123/link.txt", linkTarget: "target.txt"},
	})

	dst := t.TempDir()
	if err := extractTarGzStripped(bytes.NewReader(data), dst); err != nil {
		t.Fatalf("expected relative symlink within root to succeed, got: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("expected link.txt to exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("expected link.txt to be a symlink")
	}
}
