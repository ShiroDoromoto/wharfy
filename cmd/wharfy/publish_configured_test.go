package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// publish_configured_test.go — 名指しの publish は channels: の集合に閉じる。
//
// チャネルを畳んだ配布者は、その repo を archive して最後の formula に廃止告知を手で書く。
// wharfy がそこへ書き戻すと告知を潰し、利用者は「更新が来ている」と誤認する。復活は
// 配布者が wharfy.yaml へ書き戻したときにだけ起こる(配布は明示ゲート)。

// channels: に無いチャネルを名指し → 発行せず channel_not_configured。
func TestPublishRejectsUnconfiguredChannel(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases, script, goinstall]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})

	if res.OK {
		t.Fatalf("publishing a channel absent from channels: must fail: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrChannelNotConfigured {
		t.Fatalf("want channel_not_configured: %+v", res.Errors)
	}
	if len(store.Files) != 0 {
		t.Errorf("nothing may be written to the tap: %v", store.Files)
	}
	// 宣言した集合を見せる(何を書き戻せばよいかが分かる)。
	if !strings.Contains(res.Errors[0].Message, "releases, script, goinstall") {
		t.Errorf("error should name the declared channels: %q", res.Errors[0].Message)
	}
	validateAgainst(t, publishSchemaID, res)
}

// --yes でも拒否する(同意は「設定に書いたものを配る」同意であって、畳んだチャネルの復活ではない)。
func TestPublishRejectsUnconfiguredChannelWithYes(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [script]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"scoop"})

	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrChannelNotConfigured {
		t.Fatalf("--yes must not resurrect a folded channel: %+v", res)
	}
	if len(store.Files) != 0 {
		t.Errorf("nothing may be written: %v", store.Files)
	}
}

// 綴り違いは「設定に無い」ではなく「そんなチャネルは無い」と言う(直し方が違う)。
func TestPublishUnknownChannelStaysUnknown(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [script]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrews"})

	if res.OK {
		t.Fatalf("unknown channel must fail: %+v", res)
	}
	for _, e := range res.Errors {
		if e.Code == output.ErrChannelNotConfigured {
			t.Errorf("a typo is not a folded channel: %+v", e)
		}
	}
}

// channels: に在るチャネルは従来どおり通る(拒否が広すぎないことの証)。
func TestPublishAllowsConfiguredChannel(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})

	for _, e := range res.Errors {
		if e.Code == output.ErrChannelNotConfigured {
			t.Fatalf("declared channel must not be rejected: %+v", res)
		}
	}
}
