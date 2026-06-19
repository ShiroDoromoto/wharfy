package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// writeChannels は scratch に wharfy.yaml を置き、assess 対象を 1 チャネルに絞る(他の
// 実体照合がネットワークへ出ないようにする)。
func writeChannels(t *testing.T, root, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "wharfy.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goinstall: tag があり proxy に版がある → source=probed・published。
func TestStatusGoinstallProbed(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [goinstall]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Version":"v1.0.0"}`))
	}))
	defer srv.Close()
	defer swapGoinstallProxy(srv.URL)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	gi := findChannel(out.Channels, "goinstall")
	if gi == nil || gi.Source != state.SourceProbed || !gi.Published || gi.Version != "v1.0.0" {
		t.Fatalf("goinstall should be probed+published at v1.0.0: %+v", gi)
	}
	if gi.Drift != nil {
		t.Errorf("goinstall has no record→remote drift concept: %+v", gi.Drift)
	}
}

// goinstall: proxy に無い → not published、案内 reason。
func TestStatusGoinstallNotOnProxy(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [goinstall]\n")
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	defer swapGoinstallProxy(srv.URL)()

	out, _ := buildStatus(context.Background(), true)
	gi := findChannel(out.Channels, "goinstall")
	if gi == nil || gi.Published || gi.Source != state.SourceProbed {
		t.Fatalf("goinstall not-on-proxy should be probed+unpublished: %+v", gi)
	}
}

// script: 記録 v1.2.0 vs install.sh 上 v1.1.0 → drift(behind)。
func TestStatusScriptDrift(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [script]\n")
	chdir(t, root)

	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"script": {Version: "1.2.0", Target: "acme/demo release:install.sh", At: "t"},
	}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nVERSION=\"1.1.0\"\n"))
	}))
	defer srv.Close()
	defer swapScriptProbeURL(srv.URL + "/install.sh")()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	sc := findChannel(out.Channels, "script")
	if sc == nil || sc.Source != state.SourceDrift {
		t.Fatalf("script should be drift: %+v", sc)
	}
	if sc.Drift == nil || sc.Drift.Kind != state.DriftBehind || sc.Drift.Recorded != "1.2.0" || sc.Drift.Remote != "1.1.0" {
		t.Errorf("script drift wrong: %+v", sc.Drift)
	}
	if !hasNextDoOut(out, "wharfy publish script") {
		t.Errorf("drift should drive publish script: %+v", out.Next)
	}
}

func swapScriptProbeURL(url string) func() {
	prev := scriptProbeURL
	scriptProbeURL = url
	return func() { scriptProbeURL = prev }
}

// scoop: 記録 v1.2.0 vs bucket manifest v1.1.0 → drift(behind)。homebrew と同型。
func TestStatusScoopDrift(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [scoop]\n")
	chdir(t, root)

	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"scoop": {Version: "1.2.0", Target: "acme/scoop-demo", At: "t"},
	}
	_ = state.Save(root, st)

	store := channel.NewInMemoryTapStore()
	store.Files["bucket/demo.json"] = `{"version":"1.1.0"}`
	defer swapTapStore(store)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	sc := findChannel(out.Channels, "scoop")
	if sc == nil || sc.Source != state.SourceDrift || sc.Drift == nil || sc.Drift.Kind != state.DriftBehind {
		t.Fatalf("scoop should be drift behind: %+v", sc)
	}
	if sc.Drift.Recorded != "1.2.0" || sc.Drift.Remote != "1.1.0" {
		t.Errorf("scoop drift versions wrong: %+v", sc.Drift)
	}
}
