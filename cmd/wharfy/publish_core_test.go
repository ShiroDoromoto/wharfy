package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

type fakeCoreSubmitter struct {
	url    string
	called bool
	in     channel.CoreInput
}

func (f *fakeCoreSubmitter) Submit(_ context.Context, in channel.CoreInput) (string, error) {
	f.called = true
	f.in = in
	return f.url, nil
}

func swapCoreSubmitter(s channel.CoreSubmitter) func() {
	prev := newCoreSubmitter
	newCoreSubmitter = func(string) channel.CoreSubmitter { return s }
	return func() { newCoreSubmitter = prev }
}

// dry-run: gated/prepare、formula を見せ、Formula/<l>/<name>.rb を申請先に出す。
func TestPublishHomebrewCoreDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew-core]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew-core"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Plan[0].Action != channel.ActionPrepare || pd.Plan[0].Kind != channel.KindGated {
		t.Errorf("homebrew-core plan should be gated/prepare: %+v", pd.Plan[0])
	}
	if !strings.Contains(pd.Plan[0].OwnedArtifact, "Formula/d/demo.rb") {
		t.Errorf("should target the sharded formula path: %q", pd.Plan[0].OwnedArtifact)
	}
	if !strings.Contains(pd.Plan[0].Diff, "class Demo < Formula") {
		t.Errorf("diff should show the formula: %q", pd.Plan[0].Diff)
	}
}

// --yes: PR を出し(fake)、state に pr_open + PR を記録、gated_pending を warning。
func TestPublishHomebrewCoreApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew-core]\n")
	tagScratch(t, root, "v0.6.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	sub := &fakeCoreSubmitter{url: "https://github.com/Homebrew/homebrew-core/pull/42"}
	defer swapCoreSubmitter(sub)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew-core"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !sub.called || sub.in.Central != "Homebrew/homebrew-core" || sub.in.FormulaFile != "Formula/d/demo.rb" {
		t.Errorf("submitter should get the formula for homebrew-core: called=%v in=%+v", sub.called, sub.in)
	}
	if !hasWarning(res, "gated_pending") {
		t.Errorf("should warn gated_pending: %+v", res.Warnings)
	}
	st, _ := state.Load(root, "demo")
	rec := st.Publish["homebrew-core"]
	if rec.State != "pr_open" || rec.PR != "https://github.com/Homebrew/homebrew-core/pull/42" {
		t.Errorf("homebrew-core record should track pr_open + URL: %+v", rec)
	}
}
