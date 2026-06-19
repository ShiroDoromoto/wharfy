package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// dry-run: tag があっても、sha を含むプレビュー(homebrew)は checksums が暫定だと明示する(#4)。
func TestPublishHomebrewDryRunMarksChecksumsProvisional(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.2.3") // tag あり(tag 欠如の注記ではないことを示す)
	chdir(t, root)
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()
	defer swapTapStore(channel.NewInMemoryTapStore())()
	flagYes = false

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("dry-run ok: %+v", res)
	}
	if !strings.Contains(res.Message, "provisional") || !strings.Contains(res.Message, "--yes") {
		t.Errorf("sha-bearing preview must mark checksums provisional: %q", res.Message)
	}
}

// 対照: sha を含まない channel(script)は暫定 sha 注記を出さない。
func TestPublishScriptDryRunNoProvisionalNote(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.2.3")
	chdir(t, root)
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()
	flagYes = false

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"script"})
	if !res.OK {
		t.Fatalf("dry-run ok: %+v", res)
	}
	if strings.Contains(res.Message, "provisional") {
		t.Errorf("script has no checksums; should not claim provisional sha: %q", res.Message)
	}
}

// dry-run: tap が未作成なら tap_will_be_created で予告する(まだ作らない)。
func TestPublishHomebrewDryRunWarnsTapWillBeCreated(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()
	store := channel.NewInMemoryTapStore()
	store.RepoExists = false // tap 未作成
	defer swapTapStore(store)()
	flagYes = false

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("dry-run ok: %+v", res)
	}
	if !hasWarning(res, output.WarnTapWillBeCreated) {
		t.Errorf("should warn tap_will_be_created: %+v", res.Warnings)
	}
	if store.Created != 0 {
		t.Errorf("dry-run must not create the tap, created=%d", store.Created)
	}
}

// --yes: tap が未作成なら作成→formula 書込→created を warning で報告。
func TestPublishHomebrewApplyCreatesTap(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	store := channel.NewInMemoryTapStore()
	store.RepoExists = false // tap 未作成
	defer swapTapStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if store.Created != 1 {
		t.Errorf("tap should be created once, created=%d", store.Created)
	}
	if store.Commits != 1 {
		t.Errorf("formula should be written after create, commits=%d", store.Commits)
	}
	if !hasWarning(res, output.WarnTapWillBeCreated) {
		t.Errorf("apply should report the created tap: %+v", res.Warnings)
	}
}

// Create が失敗したら target_create_failed で停止(formula は書かない)。
type failCreateStore struct{ *channel.InMemoryTapStore }

func (f failCreateStore) Create(context.Context) error { return errString("permission denied") }

func TestPublishHomebrewCreateFails(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	inner := channel.NewInMemoryTapStore()
	inner.RepoExists = false
	store := failCreateStore{inner}
	defer swapTapStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrTargetCreateFailed {
		t.Fatalf("create failure should be target_create_failed: %+v", res)
	}
	if inner.Commits != 0 {
		t.Errorf("must not write formula when create failed, commits=%d", inner.Commits)
	}
}
