package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// release が記録済み(同 version)なら batch は ReleaseAll を再実行せず再利用する(c2)。
func TestPublishAllReusesRecordedRelease(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew, releases]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	// 先に release が成果物を記録した状態を作る。
	if err := build.SaveArtifacts(root, "1.0.0", sampleArchiveArtifacts()); err != nil {
		t.Fatal(err)
	}
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
	if mr.calls != 0 {
		t.Errorf("recorded release should be reused, not re-run; calls=%d", mr.calls)
	}
	if store.Commits != 1 {
		t.Errorf("homebrew formula should still be written, commits=%d", store.Commits)
	}
}

// state 認識の再開(b): その version で発行済みのチャネルは skip(noop)、残りだけ進む。
func TestPublishAllSkipsAlreadyPublished(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew, releases]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	// homebrew は既に v1.0.0 で発行済み(前回の途中まで成功した想定)。
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"homebrew": {Version: "1.0.0", Target: "acme/homebrew-demo", At: "t"},
	}
	_ = state.Save(root, st)
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
	items := planChannels(res.Data.(publishData))
	if items["homebrew"].Action != channel.ActionNoop {
		t.Errorf("already-published homebrew should be noop, got %+v", items["homebrew"])
	}
	// 再開なので homebrew formula は書き直さない。
	if store.Commits != 0 {
		t.Errorf("completed channel must not be rewritten on resume, commits=%d", store.Commits)
	}
}
