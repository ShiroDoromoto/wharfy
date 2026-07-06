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

// scratchBundleLinux は Linux GUI(BYO-bundle)リポを作る: AppImage ＋ 持ち込み deb/rpm。
func scratchBundleLinux(t *testing.T, channels, extra string) string {
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
	write("dist/Demo-x86_64.AppImage", "appimage-bytes")
	write("dist/demo-app_1.0_amd64.deb", "deb-bytes")
	write("dist/demo-app-1.0.x86_64.rpm", "rpm-bytes")
	write("wharfy.yaml", `project: demo
license: MIT
description: a demo desktop app
channels: [`+channels+`]
`+extra+`bundle:
  name: Demo
  bundles:
    - { os: linux, arch: amd64, kind: appimage, path: dist/Demo-x86_64.AppImage }
    - { os: linux, arch: amd64, kind: deb, path: dist/demo-app_1.0_amd64.deb }
    - { os: linux, arch: amd64, kind: rpm, path: dist/demo-app-1.0.x86_64.rpm }
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

// TestReleaseBundleUploadsAppImage: release --yes が持ち込みバンドル(AppImage/deb/rpm)を再 archive
// せずそのまま Release アセットにする → AppImage が直 DL 可能になる(依頼③)。
func TestReleaseBundleUploadsAppImage(t *testing.T) {
	root := scratchBundleLinux(t, "releases", "")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	rel := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(rel)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	up := rel.Tags["v1.0.0"]
	for _, name := range []string{"Demo-x86_64.AppImage", "demo-app_1.0_amd64.deb", "demo-app-1.0.x86_64.rpm"} {
		if _, ok := up[name]; !ok {
			t.Errorf("bundle %q should be uploaded verbatim to the release: have %v", name, up)
		}
	}
}

// TestPublishAptBundleApply: publish apt --yes が、持ち込みの GUI deb(demo-app_…)を再パッケージせず
// 同じ hosted repo へ上げる(rpm は無視・依頼③)。パッケージ名はバンドラ生成物のまま。
func TestPublishAptBundleApply(t *testing.T) {
	root := scratchBundleLinux(t, "releases, apt, rpm", "apt: { provider: fury, user: acme }\nrpm: { provider: fury, user: acme }\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "tok")

	var got []string
	defer swapUploader(func(_ context.Context, repo, token, path string) error {
		got = append(got, repo+"|"+filepath.Base(path))
		return nil
	})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	// 持ち込み .deb だけが push.fury.io へ上がる(.rpm/.AppImage は apt では無視)。
	if len(got) != 1 || got[0] != "https://push.fury.io/acme/|demo-app_1.0_amd64.deb" {
		t.Errorf("apt bundle upload wrong: %v", got)
	}
	st, _ := state.Load(root, "demo")
	if st.Publish["apt"].Version != "1.0.0" {
		t.Errorf("apt publish should be recorded @1.0.0: %+v", st.Publish["apt"])
	}
}

// TestPublishRpmBundleApply: rpm 側も対称に、持ち込み .rpm だけを上げる。
func TestPublishRpmBundleApply(t *testing.T) {
	root := scratchBundleLinux(t, "releases, apt, rpm", "apt: { provider: fury, user: acme }\nrpm: { provider: fury, user: acme }\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "tok")

	var got []string
	defer swapUploader(func(_ context.Context, repo, token, path string) error {
		got = append(got, filepath.Base(path))
		return nil
	})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"rpm"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(got) != 1 || got[0] != "demo-app-1.0.x86_64.rpm" {
		t.Errorf("rpm bundle upload wrong: %v", got)
	}
}
