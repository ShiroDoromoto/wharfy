package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// prereleaseFlag は --prerelease を立てて、テスト後に必ず戻す(グローバルな局所フラグ)。
func prereleaseFlag(t *testing.T) {
	t.Helper()
	flagPrerelease = true
	t.Cleanup(func() { flagPrerelease = false })
}

// TestReleasePrereleasePlan: --prerelease の plan は「latest にはしない」と言い、その意図を
// data と次の一手に載せる(--yes の行にも --prerelease が残る)。
func TestReleasePrereleasePlan(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	prereleaseFlag(t)

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !strings.Contains(res.Message, "prerelease") {
		t.Errorf("plan message should say it is a prerelease: %q", res.Message)
	}
	data, ok := res.Data.(releaseData)
	if !ok || !data.Prerelease {
		t.Errorf("data.prerelease should be true: %+v", res.Data)
	}
	var found bool
	for _, n := range res.Next {
		if strings.Contains(n.Do, "--prerelease") {
			found = true
		}
	}
	if !found {
		t.Errorf("next should keep --prerelease on the apply line: %+v", res.Next)
	}
}

// TestReleasePrereleaseApply: --prerelease --yes は資産を上げるが latest にはしない。
// 上げた事実(warning・台帳の印)を残すので、「配ったつもり」で止まらない。
func TestReleasePrereleaseApply(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	prereleaseFlag(t)

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	defer swapMultiReleaser(&fakeMultiReleaser{arts: sampleArchiveArtifacts()})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !store.Pre["v0.1.0"] {
		t.Error("the release must be created as a prerelease (or users get it the moment it lands)")
	}
	if len(store.Tags["v0.1.0"]) != 7 {
		t.Errorf("assets must still be uploaded (that is the whole point): %v", store.Tags["v0.1.0"])
	}
	if !hasWarning(res, output.WarnPrereleaseNotLatest) {
		t.Errorf("must warn that users are still on the old version: %v", warnCodes(res.Warnings))
	}
	st, _ := state.Load(root, "app")
	if !st.Publish["releases"].Prerelease || !st.Publish["script"].Prerelease {
		t.Errorf("the ledger must mark it as uploaded-but-not-delivered: %+v", st.Publish)
	}
}

// TestReleasePrereleaseRefusesAlreadyPublic: latest として公開済みのタグへ --prerelease で
// 上げ直そうとしたら拒む —— 上げれば利用者が今まさに落としている資産を差し替えてしまう。
func TestReleasePrereleaseRefusesAlreadyPublic(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	prereleaseFlag(t)

	store := channel.NewInMemoryReleaseStore()
	// v0.1.0 は既に「普通のリリース」として在る(prerelease ではない)。
	_ = store.Upload(context.Background(), "v0.1.0", "app 0.1.0", []channel.ReleaseAsset{{Name: "app_0.1.0_linux_amd64.tar.gz"}}, channel.ReleaseOptions{})
	defer swapReleaseStore(store)()
	defer swapMultiReleaser(&fakeMultiReleaser{arts: sampleArchiveArtifacts()})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if res.OK {
		t.Fatalf("expected a refusal: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrReleaseAlreadyPublic {
		t.Errorf("want %s: %+v", output.ErrReleaseAlreadyPublic, res.Errors)
	}
	if store.Uploads != 1 {
		t.Errorf("nothing may be uploaded after the refusal (uploads=%d)", store.Uploads)
	}
}

// TestReleaseOntoExistingPrereleaseStaysPrerelease: --prerelease 無しで上げ直しても、既存の
// prerelease は latest に変わらない(昇格は別の明示の工程)。黙って変わらないので警告で言う。
func TestReleaseOntoExistingPrereleaseStaysPrerelease(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := channel.NewInMemoryReleaseStore()
	_ = store.Upload(context.Background(), "v0.1.0", "app 0.1.0", []channel.ReleaseAsset{{Name: "old.tar.gz"}}, channel.ReleaseOptions{Prerelease: true})
	defer swapReleaseStore(store)()
	defer swapMultiReleaser(&fakeMultiReleaser{arts: sampleArchiveArtifacts()})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !store.Pre["v0.1.0"] {
		t.Error("re-uploading must not silently make an unverified prerelease the latest release")
	}
	if !hasWarning(res, output.WarnPrereleaseNotLatest) {
		t.Errorf("the release is still a prerelease and must say so: %v", warnCodes(res.Warnings))
	}
}
