package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
)

// scratchCombined は prebuilt(CLI)と bundle(GUI)を**併用**するリポを作る(依頼書四通目=依頼②)。
// CLI アーカイブと GUI バンドルの両方が同じ Release に載るべき、という回帰の土台。
func scratchCombined(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dist/app-darwin-arm64", "macos-cli")
	write("dist/app-linux-amd64", "linux-cli")
	write("dist/App.dmg", "dmg-bytes")
	write("wharfy.yaml", `project: app
license: MIT
description: a combined cli+gui project
channels: [releases, homebrew, cask, script]
prebuilt:
  binaries:
    - { os: darwin, arch: arm64, path: dist/app-darwin-arm64 }
    - { os: linux,  arch: amd64, path: dist/app-linux-amd64 }
bundle:
  name: App
  bundles:
    - { os: darwin, arch: arm64, kind: dmg, path: dist/App.dmg }
`)
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/app.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestReleaseCombinedShipsCliAndGui: prebuilt(CLI)と bundle(GUI)を併用したとき、release --yes は
// **両方**を同じ Release に載せる。従来は bundle が prebuilt を握り潰し、CLI アーカイブと install.sh が
// 黙って欠落 → publish が 404 を指す Formula を書く半端リリースになっていた(依頼②)。
func TestReleaseCombinedShipsCliAndGui(t *testing.T) {
	root := scratchCombined(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	up := store.Tags["v0.1.0"]
	// CLI アーカイブ + install.sh(prebuilt/script)と GUI バンドル(bundle)が同居する。
	for _, name := range []string{
		"app_0.1.0_darwin_arm64.tar.gz",
		"app_0.1.0_linux_amd64.tar.gz",
		"install.sh",
		"App.dmg",
	} {
		if _, ok := up[name]; !ok {
			t.Errorf("combined release missing %q (have %v)", name, up)
		}
	}
	// artifacts.json は CLI 2 本 + GUI 1 本 = 3 成果物を記録する(publish が消費する土台)。
	set, found := mustLoadArtifacts(t, root)
	if !found || set.Version != "0.1.0" || len(set.Artifacts) != 3 {
		t.Errorf("artifacts.json should record 3 artifacts (2 cli + 1 gui): found=%v %+v", found, set)
	}
}
