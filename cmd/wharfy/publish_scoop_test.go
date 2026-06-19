package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// dry-run: scoop manifest を差分で見せ、--yes と前提を案内。
func TestPublishScoopDryRun(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	flagYes = false

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"scoop"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || len(pd.Plan) != 1 || pd.Plan[0].Action != channel.ActionCreate {
		t.Fatalf("expected create plan: %+v", pd)
	}
	if pd.Plan[0].OwnedArtifact != "acme/scoop-demo:bucket/demo.json" {
		t.Errorf("owned_artifact = %q", pd.Plan[0].OwnedArtifact)
	}
	if !strings.Contains(pd.Plan[0].Diff, `"architecture"`) {
		t.Errorf("diff should show the manifest: %q", pd.Plan[0].Diff)
	}
	if res.Next[len(res.Next)-1].Do != "wharfy publish scoop --yes" {
		t.Errorf("--yes should target scoop: %+v", res.Next)
	}
	if store.Commits != 0 {
		t.Errorf("dry-run must not write, commits = %d", store.Commits)
	}
	validateAgainst(t, publishSchemaID, res)
}

// --yes: 実 release(fake)→ bucket に manifest を書き、state に scoop/releases を記録。
func TestPublishScoopApply(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v0.3.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"scoop"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !res.Data.(publishData).Applied || store.Commits != 1 {
		t.Errorf("apply should write the bucket once: applied=%v commits=%d", res.Data.(publishData).Applied, store.Commits)
	}
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["scoop"]; !ok {
		t.Error("scoop publish should be recorded")
	}
	if _, ok := st.Publish["releases"]; !ok {
		t.Error("releases should be recorded")
	}
}
