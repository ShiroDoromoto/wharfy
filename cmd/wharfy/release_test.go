package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// dry-run: tag が無ければ git tag を未充足要件として出し、書かない。
func TestReleaseDryRun(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	mr := &fakeMultiReleaser{arts: sampleArchiveArtifacts()}
	defer swapMultiReleaser(mr)()

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("dry-run ok: %+v", res)
	}
	if mr.calls != 0 {
		t.Errorf("dry-run must not release, calls=%d", mr.calls)
	}
	if _, found := mustLoadArtifacts(t, root); found {
		t.Errorf("dry-run must not write artifacts.json")
	}
	if !hasNextDo(res, "wharfy release --yes") {
		t.Errorf("next should point to --yes: %+v", res.Next)
	}
}

// --yes: release を 1 回走らせ、成果物を artifacts.json に記録し、state に releases を記録。
func TestReleaseApplyRecordsArtifacts(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v2.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	arts := sampleArchiveArtifacts()
	mr := &fakeMultiReleaser{arts: arts}
	defer swapMultiReleaser(mr)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if mr.calls != 1 {
		t.Errorf("release should run once, calls=%d", mr.calls)
	}
	set, found := mustLoadArtifacts(t, root)
	if !found || set.Version != "2.0.0" || len(set.Artifacts) != len(arts) {
		t.Errorf("artifacts.json wrong: found=%v %+v", found, set)
	}
	st, _ := state.Load(root, "demo")
	if st.Publish["releases"].Version != "2.0.0" {
		t.Errorf("state should record releases@2.0.0: %+v", st.Publish["releases"])
	}
	if !hasNextDo(res, "wharfy publish") {
		t.Errorf("next should point to publish: %+v", res.Next)
	}
}

// skipDocker=true で呼ばれること(container は release の範囲外)。
func TestReleaseApplySkipsDocker(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v2.0.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	mr := &fakeMultiReleaser{arts: sampleArchiveArtifacts()}
	defer swapMultiReleaser(mr)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK || !mr.lastSkipDocker {
		t.Errorf("release must skip docker (container is publish's job): skip=%v ok=%v", mr.lastSkipDocker, res.OK)
	}
}

func mustLoadArtifacts(t *testing.T, root string) (build.ArtifactSet, bool) {
	t.Helper()
	set, found, err := build.LoadArtifacts(root)
	if err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	return set, found
}
