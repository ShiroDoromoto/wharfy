package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// fakeSubmitter は実 GitHub に触れず固定の PR URL を返す。
type fakeSubmitter struct {
	url    string
	err    error
	called bool
	files  map[string]string
}

func (f *fakeSubmitter) Submit(_ context.Context, _ channel.WingetInput, files map[string]string) (string, error) {
	f.called = true
	f.files = files
	return f.url, f.err
}

func swapWingetSubmitter(s channel.Submitter) func() {
	prev := newWingetSubmitter
	newWingetSubmitter = func(string) channel.Submitter { return s }
	return func() { newWingetSubmitter = prev }
}

// dry-run: gated は action=prepare、manifest 3 種を見せ、前提を出す。
func TestPublishWingetDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"winget"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Plan[0].Action != channel.ActionPrepare || pd.Plan[0].Kind != channel.KindGated {
		t.Errorf("winget plan should be gated/prepare: %+v", pd.Plan[0])
	}
	if !strings.Contains(pd.Plan[0].Diff, "acme.demo.installer.yaml") {
		t.Errorf("diff should show the manifests: %q", pd.Plan[0].Diff)
	}
	if res.Next[len(res.Next)-1].Do != "wharfy publish winget --yes" {
		t.Errorf("--yes should target winget: %+v", res.Next)
	}
}

// --yes: PR を出し(fake)、state に pr_open + PR URL を記録、gated_pending を warning。
func TestPublishWingetApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	tagScratch(t, root, "v0.6.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	sub := &fakeSubmitter{url: "https://github.com/microsoft/winget-pkgs/pull/99"}
	defer swapWingetSubmitter(sub)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"winget"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !sub.called || len(sub.files) != 3 {
		t.Errorf("submitter should get 3 manifests: called=%v files=%d", sub.called, len(sub.files))
	}
	if !hasWarning(res, "gated_pending") {
		t.Errorf("should warn gated_pending: %+v", res.Warnings)
	}
	if !hasNextDo(res, "open https://github.com/microsoft/winget-pkgs/pull/99") {
		t.Errorf("should point to the PR: %+v", res.Next)
	}
	st, _ := state.Load(root, "demo")
	rec := st.Publish["winget"]
	if rec.State != "pr_open" || rec.PR != "https://github.com/microsoft/winget-pkgs/pull/99" {
		t.Errorf("winget record should track pr_open + URL: %+v", rec)
	}
}

// gated 重複 PR ガード: 既存 PR が OPEN なら新規提出しない(前回審査が未完了のケース)。
func TestPublishWingetSkipsWhenPROpen(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	tagScratch(t, root, "v0.7.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"winget": {Version: "0.6.0", Target: "acme.demo", State: "pr_open", PR: "https://github.com/microsoft/winget-pkgs/pull/99", At: "t"},
	}
	_ = state.Save(root, st)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","merged":false}`))
	}))
	defer srv.Close()
	defer swapWingetProbeBase(srv.URL)()
	sub := &fakeSubmitter{url: "https://github.com/microsoft/winget-pkgs/pull/100"}
	defer swapWingetSubmitter(sub)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"winget"})
	if sub.called {
		t.Errorf("must NOT open a second PR while one is open")
	}
	if !res.OK || !hasWarning(res, "gated_pending") {
		t.Errorf("should skip with gated_pending: %+v", res)
	}
	if !hasNextDo(res, "open https://github.com/microsoft/winget-pkgs/pull/99") {
		t.Errorf("should point to the existing open PR: %+v", res.Next)
	}
	// 既存記録は上書きしない(元の PR を保つ)。
	st2, _ := state.Load(root, "demo")
	if st2.Publish["winget"].PR != "https://github.com/microsoft/winget-pkgs/pull/99" {
		t.Errorf("record should keep the existing PR: %+v", st2.Publish["winget"])
	}
}

// 既存 PR が merged/closed なら、新バージョンの PR は出してよい。
func TestPublishWingetSubmitsWhenPRClosed(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	tagScratch(t, root, "v0.7.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"winget": {Version: "0.6.0", Target: "acme.demo", State: "pr_open", PR: "https://github.com/microsoft/winget-pkgs/pull/99", At: "t"},
	}
	_ = state.Save(root, st)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"closed","merged":true}`))
	}))
	defer srv.Close()
	defer swapWingetProbeBase(srv.URL)()
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	sub := &fakeSubmitter{url: "https://github.com/microsoft/winget-pkgs/pull/100"}
	defer swapWingetSubmitter(sub)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"winget"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !sub.called {
		t.Errorf("merged/closed prior PR → should open a new PR")
	}
}

func swapWingetProbeBase(url string) func() {
	prev := wingetProbeBase
	wingetProbeBase = url
	return func() { wingetProbeBase = prev }
}

// status: PR API を probe して実状態を反映。open → pr_open(gated_pending)。
func TestStatusWingetProbeOpen(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"winget": {Version: "0.6.0", Target: "acme.demo", State: "pr_open", PR: "https://github.com/microsoft/winget-pkgs/pull/99", At: "t"},
	}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","merged":false}`))
	}))
	defer srv.Close()
	defer swapWingetProbeBase(srv.URL)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	wg := findChannel(out.Channels, "winget")
	if wg == nil || wg.Kind != "gated" || wg.State != "pr_open" || wg.Source != state.SourceProbed {
		t.Fatalf("winget should be probed pr_open: %+v", wg)
	}
	if !hasStatusWarning(out, "gated_pending") {
		t.Errorf("pr_open should warn gated_pending: %+v", out.Warnings)
	}
}

func hasStatusWarning(out statusOutput, code string) bool {
	for _, w := range out.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// status: PR が merged → state=merged・published=true・警告なし(記録は pr_open でも実状態で上書き)。
func TestStatusWingetProbeMerged(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [winget]\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"winget": {Version: "0.6.0", Target: "acme.demo", State: "pr_open", PR: "https://github.com/microsoft/winget-pkgs/pull/99", At: "t"},
	}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"closed","merged":true}`))
	}))
	defer srv.Close()
	defer swapWingetProbeBase(srv.URL)()

	out, _ := buildStatus(context.Background(), true)
	wg := findChannel(out.Channels, "winget")
	if wg == nil || wg.State != "merged" || !wg.Published {
		t.Fatalf("winget should be merged+published: %+v", wg)
	}
}
