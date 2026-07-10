package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// scratchBundle は GUI(BYO-bundle)リポを作る: 非 Go・bundle な wharfy.yaml ＋ 持ち込み .dmg。
func scratchBundle(t *testing.T, channels string) string {
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
	write("dist/App-arm64.dmg", "arm-bundle")
	write("dist/App-amd64.dmg", "intel-bundle")
	write("wharfy.yaml", `project: app
license: MIT
description: a demo GUI
channels: [`+channels+`]
bundle:
  name: App
  bundles:
    - { os: darwin, arch: arm64, kind: dmg, path: dist/App-arm64.dmg }
    - { os: darwin, arch: amd64, kind: dmg, path: dist/App-amd64.dmg }
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

// TestPublishCaskApply: cask --yes が持ち込みバンドルを再 archive せずネイティブ upload し、
// 同一 tap の Casks/app-app.rb を書く(BYO-bundle → Cask・依頼①②)。
func TestPublishCaskApply(t *testing.T) {
	root := scratchBundle(t, "releases, cask")
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	rel := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(rel)()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"cask"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}

	// 持ち込み .dmg 2 本がそのままのファイル名で Release に上がる(再 archive しない)。
	up := rel.Tags["v0.1.0"]
	for _, name := range []string{"App-arm64.dmg", "App-amd64.dmg"} {
		if _, ok := up[name]; !ok {
			t.Errorf("bundle %q should be uploaded verbatim: have %v", name, up)
		}
	}

	// 同一 tap に cask が書かれ、実 sha・正しい版・url が入る。
	cask, ok := tap.Files["Casks/app-app.rb"]
	if !ok {
		t.Fatalf("cask not written to tap: %v", keysOf(tap.Files))
	}
	for _, want := range []string{
		`cask "app-app" do`,
		`version "0.1.0"`,
		`name "App"`,
		`app "App.app"`,
		"https://github.com/acme/app/releases/download/v0.1.0/App-arm64.dmg",
		// 非 notarized → Gatekeeper 案内(依頼⑤)。単一引用ヒアドキュメントで、
		// 配布者の文面に #{...} があっても Ruby に評価させない。
		"caveats <<~'EOS'",
	} {
		if !strings.Contains(cask, want) {
			t.Errorf("cask missing %q\n---\n%s", want, cask)
		}
	}

	// state に cask@0.1.0 と releases が記録される。
	st, _ := state.Load(root, "app")
	if st.Publish["cask"].Version != "0.1.0" {
		t.Errorf("state should record cask@0.1.0: %+v", st.Publish["cask"])
	}
}

// TestPublishCaskNotarizeAdvisory: cask は非 notarized を配るため、publish 出力に darwin_unnotarized
// の advisory を先出しする(依頼⑤)。preview でも apply でも出る。
func TestPublishCaskNotarizeAdvisory(t *testing.T) {
	root := scratchBundle(t, "releases, cask")
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)

	rel := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(rel)()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()

	// preview
	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"cask"})
	if !hasWarning(res, "darwin_unnotarized") {
		t.Errorf("preview should advise non-notarized: %+v", res.Warnings)
	}

	// apply
	t.Setenv("GITHUB_TOKEN", "tok")
	defer func() { flagYes = false }()
	flagYes = true
	res = runPublish(context.Background(), mustLookup(t, "publish"), []string{"cask"})
	if !hasWarning(res, "darwin_unnotarized") {
		t.Errorf("apply should advise non-notarized: %+v", res.Warnings)
	}
}

// TestPublishCaskPreview: cask の dry-run はローカルでバンドルを検証して差分を見せるだけで、
// アップロードも tap への書き込みもしない(差分を見せてから書く)。
func TestPublishCaskPreview(t *testing.T) {
	root := scratchBundle(t, "releases, cask")
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)

	rel := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(rel)()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"cask"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if rel.Uploads != 0 {
		t.Errorf("preview must not upload: uploads=%d", rel.Uploads)
	}
	if tap.Commits != 0 {
		t.Errorf("preview must not write the tap: commits=%d", tap.Commits)
	}
}

// TestStatusFormulaAndCaskUnified: 1 つの config が homebrew と cask を宣言すると、`wharfy status` が
// 同じ tap の Formula(app)と Cask(app-app)を 1 覧に並べ、それぞれの版を probe する(状態一元化・依頼④)。
func TestStatusFormulaAndCaskUnified(t *testing.T) {
	root := scratchBundle(t, "homebrew, cask")
	chdir(t, root)

	// 同じ tap に Formula と Cask が同居している状態を作る。
	tap := channel.NewInMemoryTapStore()
	tap.Files["Formula/app.rb"] = "class App < Formula\n  version \"1.0.0\"\nend\n"
	tap.Files["Casks/app-app.rb"] = "cask \"app-app\" do\n  version \"1.0.0\"\nend\n"
	defer swapTapStore(tap)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	hb := findChannel(out.Channels, "homebrew")
	ck := findChannel(out.Channels, "cask")
	if hb == nil || ck == nil {
		t.Fatalf("both formula and cask should appear: %+v", out.Channels)
	}
	if !hb.Published || hb.Version != "1.0.0" {
		t.Errorf("formula row wrong: %+v", hb)
	}
	if !ck.Published || ck.Version != "1.0.0" {
		t.Errorf("cask row wrong: %+v", ck)
	}
	if ck.Target != hb.Target {
		t.Errorf("formula tap %q and cask tap %q should be the same repo", hb.Target, ck.Target)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
