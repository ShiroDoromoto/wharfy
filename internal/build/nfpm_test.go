package build

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPackagePrebuiltDeb: 各 linux バイナリから deb を生成し、命名・sha・linux 限定を検証。
func TestPackagePrebuiltDeb(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "linux-amd64")
	writeFile(t, filepath.Join(root, "dist/app-linux-arm64"), "linux-arm64")

	bins := []PrebuiltBinary{
		{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"},
		{OS: "linux", Arch: "arm64", Path: "dist/app-linux-arm64"},
		{OS: "darwin", Arch: "arm64", Path: "dist/should-be-ignored"}, // linux 以外は無視
	}
	spec := PackageSpec{
		Format: "deb", Ext: ".deb", Name: "app", BinaryName: "app",
		Version: "0.1.0", Maintainer: "acme <acme@users.noreply.github.com>",
		Description: "demo", Homepage: "https://github.com/acme/app", License: "MIT",
		Depends: []string{"git"},
	}
	arts, err := PackagePrebuilt(root, ".wharfy/dist", spec, bins)
	if err != nil {
		t.Fatalf("PackagePrebuilt: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d packages, want 2 (linux only)", len(arts))
	}
	for _, a := range arts {
		if filepath.Ext(a.Path) != ".deb" {
			t.Errorf("path %s should end in .deb", a.Path)
		}
		if a.SHA256 == "" {
			t.Errorf("missing sha for %s", a.Path)
		}
		full := filepath.Join(root, a.Path)
		info, err := os.Stat(full)
		if err != nil || info.Size() == 0 {
			t.Errorf("deb not written or empty: %s (%v)", a.Path, err)
		}
	}
	// 命名が expectedPackages と一致する
	want := map[string]bool{"app_0.1.0_linux_amd64.deb": true, "app_0.1.0_linux_arm64.deb": true}
	for _, a := range arts {
		if !want[filepath.Base(a.Path)] {
			t.Errorf("unexpected package name %s", filepath.Base(a.Path))
		}
	}
}

// TestPackagePrebuiltRpm: rpm フォーマットも生成できる。
func TestPackagePrebuiltRpm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "linux-amd64")
	spec := PackageSpec{
		Format: "rpm", Ext: ".rpm", Name: "app", BinaryName: "app",
		Version: "0.1.0", Maintainer: "acme <a@b.c>", License: "MIT",
	}
	arts, err := PackagePrebuilt(root, ".wharfy/dist", spec, []PrebuiltBinary{
		{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"},
	})
	if err != nil {
		t.Fatalf("PackagePrebuilt rpm: %v", err)
	}
	if len(arts) != 1 || filepath.Base(arts[0].Path) != "app_0.1.0_linux_amd64.rpm" {
		t.Fatalf("rpm wrong: %+v", arts)
	}
}

// TestPackagePrebuiltMissingBinary: 存在しないパスは build_failed。
func TestPackagePrebuiltMissingBinary(t *testing.T) {
	root := t.TempDir()
	spec := PackageSpec{Format: "deb", Ext: ".deb", Name: "app", BinaryName: "app", Version: "0.1.0", Maintainer: "a <a@b.c>"}
	_, err := PackagePrebuilt(root, ".wharfy/dist", spec, []PrebuiltBinary{{OS: "linux", Arch: "amd64", Path: "dist/nope"}})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}
