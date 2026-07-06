package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// scratchBundleWin は Windows GUI(BYO-bundle)リポを作る: 持ち込みポータブル zip。
func scratchBundleWin(t *testing.T, channels string) string {
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
	write("dist/Demo-x64.zip", "win-zip-bytes")
	write("wharfy.yaml", `project: demo
license: MIT
description: a demo desktop app
channels: [`+channels+`]
bundle:
  name: Demo
  bundles:
    - { os: windows, arch: amd64, kind: zip, path: dist/Demo-x64.zip }
`)
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/demo.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestPublishScoopBundleApply: scoop --yes が、持ち込み zip を app manifest(bucket/demo-app.json)に
// して bucket に書く。bin/shortcuts 付き、url は持ち込みファイル名そのまま(依頼③)。
func TestPublishScoopBundleApply(t *testing.T) {
	root := scratchBundleWin(t, "releases, scoop")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	rel := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(rel)()
	bucket := channel.NewInMemoryTapStore()
	defer swapTapStore(bucket)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"scoop"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	manifest, ok := bucket.Files["bucket/demo-app.json"]
	if !ok {
		t.Fatalf("scoop app manifest not written: %v", keysOf(bucket.Files))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatalf("manifest invalid: %v\n%s", err, manifest)
	}
	if m["version"] != "1.0.0" || m["bin"] != "Demo.exe" {
		t.Errorf("manifest fields wrong: version=%v bin=%v", m["version"], m["bin"])
	}
	a64 := m["architecture"].(map[string]any)["64bit"].(map[string]any)
	if !strings.HasSuffix(a64["url"].(string), "/releases/download/v1.0.0/Demo-x64.zip") {
		t.Errorf("url should reference the verbatim bundle: %v", a64["url"])
	}
	st, _ := state.Load(root, "demo")
	if st.Publish["scoop"].Version != "1.0.0" {
		t.Errorf("scoop publish should be recorded @1.0.0: %+v", st.Publish["scoop"])
	}
}

// TestPublishWingetBundlePreview: winget dry-run(bundle)は持ち込み zip を installer にした manifest を
// 見せ、アップロード・PR はしない(gated・PR 準備のみ・依頼③)。
func TestPublishWingetBundlePreview(t *testing.T) {
	root := scratchBundleWin(t, "releases, winget")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"winget"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || pd.Plan[0].Action != channel.ActionPrepare {
		t.Errorf("winget preview should be prepare/not-applied: %+v", pd.Plan[0])
	}
	if !strings.Contains(pd.Plan[0].Diff, "Demo-x64.zip") {
		t.Errorf("installer manifest should reference the verbatim bundle zip:\n%s", pd.Plan[0].Diff)
	}
}
