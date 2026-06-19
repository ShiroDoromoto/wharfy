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

type fakeAurPusher struct {
	called bool
	files  map[string]string
}

func (f *fakeAurPusher) Push(_ context.Context, _ string, files map[string]string) (string, error) {
	f.called = true
	f.files = files
	return "aurcommit", nil
}

func swapAurPusher(p channel.AurPusher) func() {
	prev := newAurPusher
	newAurPusher = func(string) channel.AurPusher { return p }
	return func() { newAurPusher = prev }
}

func swapAurRPCBase(url string) func() {
	prev := aurRPCBase
	aurRPCBase = url
	return func() { aurRPCBase = prev }
}

// dry-run: PKGBUILD を見せ、前提(tag/GITHUB_TOKEN/AUR_SSH_KEY)を出す。既定 pkg は <project>-bin。
func TestPublishAurDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [aur]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("AUR_SSH_KEY", "")
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"aur"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Plan[0].OwnedArtifact != "aur:demo-bin" {
		t.Errorf("default package should be <project>-bin: %q", pd.Plan[0].OwnedArtifact)
	}
	if !strings.Contains(pd.Plan[0].Diff, "pkgname=demo-bin") {
		t.Errorf("diff should show PKGBUILD: %q", pd.Plan[0].Diff)
	}
	if !requirementUnmet(pd.Requires, "AUR_SSH_KEY") {
		t.Errorf("AUR_SSH_KEY should be an unmet requirement: %+v", pd.Requires)
	}
}

// --yes: linux tarball の実 sha(fake release)→ PKGBUILD/.SRCINFO を AUR へ push(fake)→記録。
func TestPublishAurApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [aur]\n")
	tagScratch(t, root, "v0.7.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AUR_SSH_KEY", "KEY")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	pusher := &fakeAurPusher{}
	defer swapAurPusher(pusher)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"aur"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !pusher.called || pusher.files["PKGBUILD"] == "" || pusher.files[".SRCINFO"] == "" {
		t.Errorf("pusher should get PKGBUILD + .SRCINFO: called=%v files=%v", pusher.called, len(pusher.files))
	}
	if !hasNextDo(res, "yay -S demo-bin") {
		t.Errorf("should advise the install command: %+v", res.Next)
	}
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["aur"]; !ok {
		t.Error("aur publish should be recorded")
	}
}

// --yes: AUR_SSH_KEY 無し → token_missing で停止(push しない)。
func TestPublishAurNeedsKey(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [aur]\n")
	tagScratch(t, root, "v0.7.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AUR_SSH_KEY", "")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	pusher := &fakeAurPusher{}
	defer swapAurPusher(pusher)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"aur"})
	if res.OK || len(res.Errors) == 0 {
		t.Fatalf("expected failure without AUR_SSH_KEY: %+v", res)
	}
	if pusher.called {
		t.Error("must not push without the key")
	}
}

// status: AUR RPC で記録 v1.2.0 vs 実体 v1.1.0 → drift(behind)。
func TestStatusAurDrift(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [aur]\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"aur": {Version: "1.2.0", Target: "demo-bin", At: "t"}}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"Version":"1.1.0-1"}]}`))
	}))
	defer srv.Close()
	defer swapAurRPCBase(srv.URL)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	a := findChannel(out.Channels, "aur")
	if a == nil || a.Source != state.SourceDrift || a.Drift == nil || a.Drift.Kind != state.DriftBehind {
		t.Fatalf("aur should be drift behind: %+v", a)
	}
}
