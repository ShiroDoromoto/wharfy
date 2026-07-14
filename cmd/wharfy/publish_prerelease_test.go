package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// prereleaseOnStore は v0.1.0 が prerelease として上がっている状態にする。
func prereleaseOnStore(t *testing.T) *channel.InMemoryReleaseStore {
	t.Helper()
	s := channel.NewInMemoryReleaseStore()
	if err := s.Upload(context.Background(), "v0.1.0", "app 0.1.0",
		[]channel.ReleaseAsset{{Name: "app_0.1.0_linux_amd64.tar.gz"}}, channel.ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPublishRefusesPrerelease: 昇格していない版は各チャネルへ流さない。流せば利用者はその版を
// 掴み、prerelease で開けた「見せずに検証する窓」が消える。
func TestPublishRefusesPrerelease(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := prereleaseOnStore(t)
	defer swapReleaseStore(store)()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if res.OK {
		t.Fatalf("expected a refusal: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrPrereleaseNotPromoted {
		t.Errorf("want %s: %+v", output.ErrPrereleaseNotPromoted, res.Errors)
	}
	if tap.Commits != 0 {
		t.Error("nothing may be written to the tap while the release is unpromoted")
	}
}

// TestPublishAllowPrerelease: 明示すれば流せる(段階的公開・ベータ)。禁止ではなく明示の口。
func TestPublishAllowPrerelease(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := prereleaseOnStore(t)
	defer swapReleaseStore(store)()
	defer swapMultiReleaser(&fakeMultiReleaser{arts: sampleArchiveArtifacts()})()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()
	defer func() { flagYes, flagAllowPrerelease = false, false }()
	flagYes, flagAllowPrerelease = true, true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("--allow-prerelease should let it through: %+v", res)
	}
	if tap.Commits == 0 {
		t.Error("the formula should have been written")
	}
}

// TestPublishPlanWarnsPrerelease: plan は通信しない。手元の台帳が「上げただけ」と言っているなら、
// --yes が拒まれることを先に言う(叩いてから驚かせない)。
func TestPublishPlanWarnsPrerelease(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)

	st, _ := state.Load(root, "app")
	st.Publish = map[string]state.PublishRecord{
		"releases": {Version: "0.1.0", Target: "acme/app", At: "2026-01-01T00:00:00Z", Prerelease: true},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}

	res := runPublish(context.Background(), mustLookup(t, "publish"), nil)
	if !res.OK {
		t.Fatalf("plan should stay green: %+v", res)
	}
	if !hasWarning(res, output.WarnPrereleaseNotLatest) {
		t.Errorf("plan should say the version is not promoted yet: %v", warnCodes(res.Warnings))
	}
}
