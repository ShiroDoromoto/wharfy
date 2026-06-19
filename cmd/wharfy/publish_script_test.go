package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// dry-run: install.sh の内容を差分で見せ、--yes と前提(tag/token)を案内。
func TestPublishScriptDryRun(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"script"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || len(pd.Plan) != 1 || pd.Plan[0].Action != channel.ActionCreate {
		t.Fatalf("expected create plan, not applied: %+v", pd)
	}
	if !strings.Contains(pd.Plan[0].OwnedArtifact, "release:install.sh") {
		t.Errorf("owned_artifact should be the release install.sh: %q", pd.Plan[0].OwnedArtifact)
	}
	if !strings.Contains(pd.Plan[0].Diff, "#!/bin/sh") {
		t.Errorf("diff should show the install.sh content: %q", pd.Plan[0].Diff[:min(40, len(pd.Plan[0].Diff))])
	}
	if res.Next[len(res.Next)-1].Do != "wharfy publish script --yes" {
		t.Errorf("--yes should target script: %+v", res.Next)
	}
	validateAgainst(t, publishSchemaID, res)
}

// --yes: 実 release(fake)で install.sh を同梱アップロードし、state に script/releases を記録。
func TestPublishScriptApply(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v0.2.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"script"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if !pd.Applied {
		t.Errorf("expected applied=true: %+v", pd)
	}
	if !hasNextDo(res, "curl -fsSL https://github.com/acme/demo/releases/latest/download/install.sh | sh") {
		t.Errorf("should advise the curl install command: %+v", res.Next)
	}
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["script"]; !ok {
		t.Error("script publish should be recorded")
	}
	if _, ok := st.Publish["releases"]; !ok {
		t.Error("releases should be recorded")
	}
}
