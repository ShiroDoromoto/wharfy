package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeRun は GoReleaser サブプロセスの代わりに dist/artifacts.json と成果物ファイルを置く。
func fakeRun(root, dist string, entries []map[string]any, files map[string]string) Runner {
	return func(_ context.Context, _ string, _ string, _ ...string) ([]byte, error) {
		for rel, content := range files {
			full := filepath.Join(root, rel)
			_ = os.MkdirAll(filepath.Dir(full), 0o755)
			_ = os.WriteFile(full, []byte(content), 0o755)
		}
		b, _ := json.Marshal(entries)
		_ = os.MkdirAll(filepath.Join(root, dist), 0o755)
		_ = os.WriteFile(filepath.Join(root, dist, "artifacts.json"), b, 0o644)
		return []byte("snapshot ok"), nil
	}
}

func okLookPath(string) (string, error) { return "/usr/bin/goreleaser", nil }

func TestBuildParsesBinariesWithSha256(t *testing.T) {
	root := t.TempDir()
	dist := "out"
	binRel := "out/tool_linux_amd64/tool"
	content := "fake-elf-binary"
	entries := []map[string]any{
		{"path": binRel, "goos": "linux", "goarch": "amd64", "type": "Binary"},
		{"path": "out/archive.tar.gz", "goos": "linux", "goarch": "amd64", "type": "Archive"}, // 無視される
	}
	b := &GoReleaserBuilder{
		Bin: "goreleaser", DistDir: dist, LookPath: okLookPath,
		Run: fakeRun(root, dist, entries, map[string]string{binRel: content}),
	}

	arts, err := b.Build(context.Background(), root, "cfg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 Binary artifact, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	sum := sha256.Sum256([]byte(content))
	if a.OS != "linux" || a.Arch != "amd64" || a.Path != binRel {
		t.Errorf("artifact fields wrong: %+v", a)
	}
	if a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want %q", a.SHA256, hex.EncodeToString(sum[:]))
	}
}

func TestBuildUnavailableWhenBinMissing(t *testing.T) {
	b := &GoReleaserBuilder{
		Bin:      "goreleaser",
		DistDir:  "out",
		LookPath: func(string) (string, error) { return "", errors.New("not found in PATH") },
		Run:      func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil },
	}
	_, err := b.Build(context.Background(), t.TempDir(), "cfg.yaml")
	var unavail *UnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("want UnavailableError, got %v", err)
	}
}

func TestBuildFailedWhenRunErrors(t *testing.T) {
	b := &GoReleaserBuilder{
		Bin: "goreleaser", DistDir: "out", LookPath: okLookPath,
		Run: func(context.Context, string, string, ...string) ([]byte, error) {
			return []byte("build error: darwin/arm64 failed"), errors.New("exit status 1")
		},
	}
	_, err := b.Build(context.Background(), t.TempDir(), "cfg.yaml")
	var failed *FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("want FailedError, got %v", err)
	}
	if failed.Output == "" {
		t.Error("FailedError should carry the build output for diagnosis")
	}
}

func TestBuildFailedWhenArtifactsMissing(t *testing.T) {
	root := t.TempDir()
	b := &GoReleaserBuilder{
		Bin: "goreleaser", DistDir: "out", LookPath: okLookPath,
		Run: func(context.Context, string, string, ...string) ([]byte, error) { return []byte("ok"), nil }, // artifacts.json を作らない
	}
	_, err := b.Build(context.Background(), root, "cfg.yaml")
	var failed *FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("want FailedError on missing artifacts.json, got %v", err)
	}
}
