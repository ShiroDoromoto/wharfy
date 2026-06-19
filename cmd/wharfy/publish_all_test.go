package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

type fakeMultiReleaser struct {
	arts           []build.Artifact
	calls          int
	err            error
	lastSkipDocker bool
}

func (f *fakeMultiReleaser) ReleaseAll(_ context.Context, _, _ string, skipDocker bool) ([]build.Artifact, error) {
	f.calls++
	f.lastSkipDocker = skipDocker
	return f.arts, f.err
}

func swapMultiReleaser(m build.MultiReleaser) func() {
	prev := newMultiReleaser
	newMultiReleaser = func(string) build.MultiReleaser { return m }
	return func() { newMultiReleaser = prev }
}

func planChannels(pd publishData) map[string]channel.PlanItem {
	m := map[string]channel.PlanItem{}
	for _, p := range pd.Plan {
		m[p.Channel] = p
	}
	return m
}

// 引数なし publish (dry-run): 全構成チャネルのサマリ plan を 1 度に出す(release は走らせない)。
func TestPublishAllDryRun(t *testing.T) {
	root := scratchModule(t) // 既定 channels = homebrew, scoop, releases, script, goinstall
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	mr := &fakeMultiReleaser{}
	defer swapMultiReleaser(mr)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	items := planChannels(pd)
	for _, ch := range []string{"homebrew", "scoop", "releases", "script", "goinstall"} {
		if _, ok := items[ch]; !ok {
			t.Errorf("summary missing channel %q (have %v)", ch, pd.Plan)
		}
	}
	if items["homebrew"].OwnedArtifact != "acme/homebrew-demo:Formula/demo.rb" {
		t.Errorf("homebrew artifact = %q", items["homebrew"].OwnedArtifact)
	}
	if !requirementUnmet(pd.Requires, "GITHUB_TOKEN") {
		t.Errorf("GITHUB_TOKEN should be an unmet requirement: %+v", pd.Requires)
	}
	if !hasNextDo(res, "wharfy publish --yes") {
		t.Errorf("should guide to --yes (all): %+v", res.Next)
	}
	if mr.calls != 0 {
		t.Errorf("dry-run must not run a release, calls = %d", mr.calls)
	}
}

// 引数なし publish --yes: ReleaseAll を 1 回だけ走らせ、各チャネルを書く。
func TestPublishAllApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew, releases, script, goinstall]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	mr := &fakeMultiReleaser{arts: sampleArchiveArtifacts()}
	defer swapMultiReleaser(mr)()
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if mr.calls != 1 {
		t.Errorf("release must run exactly once for the whole batch, calls = %d", mr.calls)
	}
	if !res.Data.(publishData).Applied {
		t.Errorf("expected applied")
	}
	if store.Commits != 1 {
		t.Errorf("homebrew formula should be written once, commits = %d", store.Commits)
	}
	st, _ := state.Load(root, "demo")
	for _, ch := range []string{"homebrew", "releases", "script"} {
		if _, ok := st.Publish[ch]; !ok {
			t.Errorf("state should record %q", ch)
		}
	}
	if !hasNextDo(res, "wharfy verify") {
		t.Errorf("should guide to verify: %+v", res.Next)
	}
}

// apt/aur 等トークン不足のチャネルは batch を止めず skip + channel_skipped 警告。
func TestPublishAllSkipsUnconfigured(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew, aur]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AUR_SSH_KEY", "") // aur は鍵なし → skip
	defer swapMultiReleaser(&fakeMultiReleaser{arts: sampleArchiveArtifacts()})()
	defer swapTapStore(channel.NewInMemoryTapStore())()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), nil)
	if !res.OK {
		t.Fatalf("batch should succeed, skipping aur: %+v", res)
	}
	items := planChannels(res.Data.(publishData))
	if items["aur"].Action != channel.ActionSkip {
		t.Errorf("aur should be skipped (no key): %+v", items["aur"])
	}
	if items["homebrew"].Action != channel.ActionUpdate {
		t.Errorf("homebrew should still be applied: %+v", items["homebrew"])
	}
	if !hasWarning(res, "channel_skipped") {
		t.Errorf("should warn channel_skipped for aur: %+v", res.Warnings)
	}
}
