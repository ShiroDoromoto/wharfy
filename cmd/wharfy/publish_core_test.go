package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
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

func swapSourceTarballSHA(sha string) func() {
	prev := sourceTarballSHA
	sourceTarballSHA = func(context.Context, string) (string, error) { return sha, nil }
	return func() { sourceTarballSHA = prev }
}

// dry-run: gated/prepare、source-build formula を見せ、受け入れ基準＋同意要件を出す。
func TestPublishHomebrewCoreDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew-core]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew-core"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Plan[0].Action != channel.ActionPrepare || pd.Plan[0].Kind != channel.KindGated {
		t.Errorf("homebrew-core plan should be gated/prepare: %+v", pd.Plan[0])
	}
	// source-build formula(ソースから go build)であること。
	if !strings.Contains(pd.Plan[0].Diff, "depends_on \"go\" => :build") || !strings.Contains(pd.Plan[0].Diff, "archive/refs/tags/v0.0.0.tar.gz") {
		t.Errorf("diff should be a source-build formula: %q", pd.Plan[0].Diff)
	}
	if !requirementUnmet(pd.Requires, "acknowledge-review") {
		t.Errorf("dry-run should require acknowledge-review: %+v", pd.Requires)
	}
	if !hasWarning(res, "gated_pending") {
		t.Errorf("should surface acceptance criteria up front: %+v", res.Warnings)
	}
	if !hasNextDo(res, "brew audit --new --strict demo") {
		t.Errorf("should steer to brew audit first: %+v", res.Next)
	}
}

// --yes だが --acknowledge-review なし → consent_required で停止(出さない・コミュニティ配慮)。
func TestPublishHomebrewCoreNeedsAck(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew-core]\n")
	tagScratch(t, root, "v0.6.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	sub := &fakeCoreSubmitter{url: "https://github.com/Homebrew/homebrew-core/pull/42"}
	defer swapCoreSubmitter(sub)()
	defer func() { flagYes = false }()
	flagYes = true // ack なし

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew-core"})
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrConsentRequired {
		t.Fatalf("missing ack should be consent_required: %+v", res)
	}
	if sub.called {
		t.Error("must not submit a PR without acknowledgement")
	}
}

// --yes --acknowledge-review: source-build formula で PR を出し、pr_open + PR を記録。
func TestPublishHomebrewCoreApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [homebrew-core]\n")
	tagScratch(t, root, "v0.6.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	sub := &fakeCoreSubmitter{url: "https://github.com/Homebrew/homebrew-core/pull/42"}
	defer swapCoreSubmitter(sub)()
	defer swapSourceTarballSHA("deadbeef")()
	defer func() { flagYes = false; flagAckReview = false }()
	flagYes = true
	flagAckReview = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew-core"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !sub.called || sub.in.FormulaFile != "Formula/d/demo.rb" {
		t.Errorf("submitter should get the sharded formula: called=%v in=%+v", sub.called, sub.in)
	}
	if !strings.Contains(sub.in.Formula, `sha256 "deadbeef"`) || !strings.Contains(sub.in.Formula, "depends_on \"go\" => :build") {
		t.Errorf("formula should be source-build with the computed sha: %q", sub.in.Formula)
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
