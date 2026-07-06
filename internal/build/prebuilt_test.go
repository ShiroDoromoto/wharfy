package build

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func sha256Of(t *testing.T, s string) string {
	t.Helper()
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestPrebuiltBuilderValidates: 存在する持ち込みバイナリを sha256 付き Artifact にする。
func TestPrebuiltBuilderValidates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-darwin-arm64"), "macos-bin")
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "linux-bin")

	b := &PrebuiltBuilder{Binaries: []PrebuiltBinary{
		{OS: "darwin", Arch: "arm64", Path: "dist/app-darwin-arm64"},
		{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"},
	}}
	arts, err := b.Build(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(arts))
	}
	if arts[0].SHA256 != sha256Of(t, "macos-bin") {
		t.Errorf("darwin sha = %s", arts[0].SHA256)
	}
}

// TestPrebuiltBuilderMissing: 存在しないパスは build_failed。
func TestPrebuiltBuilderMissing(t *testing.T) {
	root := t.TempDir()
	b := &PrebuiltBuilder{Binaries: []PrebuiltBinary{{OS: "linux", Arch: "amd64", Path: "dist/nope"}}}
	if _, err := b.Build(context.Background(), root, ""); err == nil {
		t.Fatal("expected error for missing binary")
	}
}

// TestPrebuiltBuilderShaMismatch: 宣言 sha と実ファイルが食い違えば拒否。
func TestPrebuiltBuilderShaMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app"), "real")
	b := &PrebuiltBuilder{Binaries: []PrebuiltBinary{
		{OS: "linux", Arch: "amd64", Path: "dist/app", SHA256: "deadbeef"},
	}}
	if _, err := b.Build(context.Background(), root, ""); err == nil {
		t.Fatal("expected sha mismatch error")
	}
}

// TestArchivePrebuiltTarGz: unix は tar.gz、命名 <project>_<version>_<os>_<arch>.tar.gz、
// 中の実行ファイル名は binaryName。sha256 は実アーカイブのもの。
func TestArchivePrebuiltTarGz(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "linux-bin-contents")
	bins := []PrebuiltBinary{{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"}}

	arts, err := ArchivePrebuilt(root, ".wharfy/dist", "app", "0.1.0", "app", bins)
	if err != nil {
		t.Fatalf("ArchivePrebuilt: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	wantName := "app_0.1.0_linux_amd64.tar.gz"
	if filepath.Base(arts[0].Path) != wantName {
		t.Errorf("archive path = %s, want basename %s", arts[0].Path, wantName)
	}
	full := filepath.Join(root, arts[0].Path)
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	// アーカイブの sha256 が Artifact と一致する
	if got := fileSHA(t, full); got != arts[0].SHA256 {
		t.Errorf("artifact sha %s != file sha %s", arts[0].SHA256, got)
	}
	// 中身: inner 名 "app"、内容一致
	if name, content := firstTarEntry(t, full); name != "app" || content != "linux-bin-contents" {
		t.Errorf("tar entry = (%q,%q), want (app, linux-bin-contents)", name, content)
	}
}

// TestArchivePrebuiltZipWindows: windows は zip、inner に .exe が付く。
func TestArchivePrebuiltZipWindows(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-windows-amd64.exe"), "win-bin")
	bins := []PrebuiltBinary{{OS: "windows", Arch: "amd64", Path: "dist/app-windows-amd64.exe"}}

	arts, err := ArchivePrebuilt(root, ".wharfy/dist", "app", "0.1.0", "app", bins)
	if err != nil {
		t.Fatalf("ArchivePrebuilt: %v", err)
	}
	if filepath.Base(arts[0].Path) != "app_0.1.0_windows_amd64.zip" {
		t.Errorf("archive = %s", arts[0].Path)
	}
	if name := firstZipEntry(t, filepath.Join(root, arts[0].Path)); name != "app.exe" {
		t.Errorf("zip entry = %q, want app.exe", name)
	}
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func firstTarEntry(t *testing.T, path string) (name, content string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(tr)
	return hdr.Name, string(b)
}

func firstZipEntry(t *testing.T, path string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) == 0 {
		t.Fatal("empty zip")
	}
	return zr.File[0].Name
}
