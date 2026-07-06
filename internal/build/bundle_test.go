package build

import (
	"path/filepath"
	"testing"
)

// TestValidateBundlesChecksums: 持ち込みバンドルを再 archive せず、Kind を保ったまま実 sha256 付き
// Artifact にする(Path はそのまま・relay)。
func TestValidateBundlesChecksums(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/App-arm64.dmg"), "arm-bundle")
	writeFile(t, filepath.Join(root, "dist/App-amd64.dmg"), "intel-bundle")

	arts, err := ValidateBundles(root, []Bundle{
		{OS: "darwin", Arch: "arm64", Kind: "dmg", Path: "dist/App-arm64.dmg"},
		{OS: "darwin", Arch: "amd64", Kind: "dmg", Path: "dist/App-amd64.dmg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(arts))
	}
	if arts[0].Kind != "dmg" || arts[0].Path != "dist/App-arm64.dmg" {
		t.Errorf("artifact should keep kind and path verbatim: %+v", arts[0])
	}
	if arts[0].SHA256 != sha256Of(t, "arm-bundle") {
		t.Errorf("sha256 = %q, want %q", arts[0].SHA256, sha256Of(t, "arm-bundle"))
	}
}

// TestValidateBundlesSHAMismatch: 宣言 sha256 と実体が食い違えばエラー(持ち込み検証)。
func TestValidateBundlesSHAMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/App.dmg"), "real-bundle")
	_, err := ValidateBundles(root, []Bundle{
		{OS: "darwin", Arch: "universal", Kind: "dmg", Path: "dist/App.dmg", SHA256: "deadbeef"},
	})
	if err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
}

// TestValidateBundlesMissingFile: 宣言したバンドルが無ければエラー(存在検証)。
func TestValidateBundlesMissingFile(t *testing.T) {
	root := t.TempDir()
	_, err := ValidateBundles(root, []Bundle{
		{OS: "darwin", Arch: "arm64", Kind: "dmg", Path: "dist/none.dmg"},
	})
	if err == nil {
		t.Fatal("expected missing-file error")
	}
}

// TestValidateBundlesEmpty: バンドル未宣言はエラー(bundle モードで空は成立しない)。
func TestValidateBundlesEmpty(t *testing.T) {
	if _, err := ValidateBundles(t.TempDir(), nil); err == nil {
		t.Fatal("expected error for no bundles")
	}
}
