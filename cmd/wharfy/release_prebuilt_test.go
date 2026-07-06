package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// scratchPrebuilt は非 Go リポ(go.mod 無し)＋ prebuilt な wharfy.yaml ＋ ビルド済みバイナリを作る。
func scratchPrebuilt(t *testing.T) string {
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
	write("dist/app-darwin-arm64", "macos-bin")
	write("dist/app-linux-amd64", "linux-bin")
	write("dist/app-windows-amd64.exe", "win-bin")
	write("wharfy.yaml", `project: app
license: MIT
channels: [releases, homebrew, scoop, script]
prebuilt:
  binaries:
    - { os: darwin,  arch: arm64, path: dist/app-darwin-arm64 }
    - { os: linux,   arch: amd64, path: dist/app-linux-amd64 }
    - { os: windows, arch: amd64, path: dist/app-windows-amd64.exe }
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

func swapReleaseStore(s channel.ReleaseStore) func() {
	prev := newReleaseStore
	newReleaseStore = func(string, string, string) channel.ReleaseStore { return s }
	return func() { newReleaseStore = prev }
}

// scratchPrebuiltChannels は指定 channels の prebuilt リポ(linux 2 arch)を作る。
func scratchPrebuiltChannels(t *testing.T, channels string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("dist/app-linux-amd64", "linux-amd64-bin")
	write("dist/app-linux-arm64", "linux-arm64-bin")
	write("wharfy.yaml", "project: app\nlicense: MIT\nchannels: ["+channels+"]\nprebuilt:\n  binaries:\n"+
		"    - { os: linux, arch: amd64, path: dist/app-linux-amd64 }\n"+
		"    - { os: linux, arch: arm64, path: dist/app-linux-arm64 }\n")
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

// TestPublishAurPrebuiltApply: BYO-binary の aur --yes が、自前 archive+ネイティブ upload で
// linux tarball を上げ、その実 sha を参照する PKGBUILD/.SRCINFO を AUR へ push する(GoReleaser 不使用)。
func TestPublishAurPrebuiltApply(t *testing.T) {
	root := scratchPrebuiltChannels(t, "aur")
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AUR_SSH_KEY", "KEY")

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	// GoReleaser 経路(Releaser)が呼ばれたら失敗と分かるよう監視。
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	pusher := &fakeAurPusher{}
	defer swapAurPusher(pusher)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"aur"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !pusher.called || pusher.files["PKGBUILD"] == "" || pusher.files[".SRCINFO"] == "" {
		t.Errorf("pusher should get PKGBUILD + .SRCINFO: called=%v files=%d", pusher.called, len(pusher.files))
	}
	// ネイティブ Release ストアに linux archive 2 本が上がる(GoReleaser を通していない)。
	up := store.Tags["v0.1.0"]
	if _, ok := up["app_0.1.0_linux_amd64.tar.gz"]; !ok {
		t.Errorf("linux amd64 tarball should be uploaded natively: %v", up)
	}
	if _, ok := up["app_0.1.0_linux_arm64.tar.gz"]; !ok {
		t.Errorf("linux arm64 tarball should be uploaded natively: %v", up)
	}
}

// TestReleasePrebuiltApply: BYO-binary の release --yes が archive を作り、install.sh を同梱し、
// ネイティブ Release ストアへ全アセットを上げ、artifacts.json / state に記録する(GoReleaser 不使用)。
func TestReleasePrebuiltApply(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	// GoReleaser 経路が呼ばれたら失敗と分かるよう、MultiReleaser も差し替えて監視する。
	mr := &fakeMultiReleaser{arts: sampleArchiveArtifacts()}
	defer swapMultiReleaser(mr)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if mr.calls != 0 {
		t.Errorf("prebuilt release must NOT call GoReleaser, calls=%d", mr.calls)
	}
	// 3 archive + install.sh = 4 アセットが v0.1.0 に上がる。
	uploaded := store.Tags["v0.1.0"]
	if len(uploaded) != 4 {
		t.Fatalf("uploaded assets = %v, want 4 (3 archives + install.sh)", uploaded)
	}
	for _, name := range []string{
		"app_0.1.0_darwin_arm64.tar.gz",
		"app_0.1.0_linux_amd64.tar.gz",
		"app_0.1.0_windows_amd64.zip",
		"install.sh",
	} {
		if _, ok := uploaded[name]; !ok {
			t.Errorf("missing uploaded asset %q (have %v)", name, uploaded)
		}
	}
	// publish が消費する artifacts.json に archive 成果物が記録される。
	set, found := mustLoadArtifacts(t, root)
	if !found || set.Version != "0.1.0" || len(set.Artifacts) != 3 {
		t.Errorf("artifacts.json wrong: found=%v %+v", found, set)
	}
	st, _ := state.Load(root, "app")
	if st.Publish["releases"].Version != "0.1.0" {
		t.Errorf("state should record releases@0.1.0: %+v", st.Publish["releases"])
	}
	// install.sh が実際に .wharfy/ に生成される。
	if _, err := os.Stat(filepath.Join(root, ".wharfy", "install.sh")); err != nil {
		t.Errorf("install.sh not generated: %v", err)
	}
}
