package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// TestStatusReleasesPrerelease: 上げただけの版を「配った」と言わない。status は実体を見て
// prerelease だと言い、次の一手(検証 → 昇格)を出す。
func TestStatusReleasesPrerelease(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases]\n")
	tagScratch(t, root, "v1.2.0")
	chdir(t, root)

	store := channel.NewInMemoryReleaseStore()
	if err := store.Upload(context.Background(), "v1.2.0", "demo 1.2.0",
		[]channel.ReleaseAsset{{Name: "demo_1.2.0_linux_amd64.tar.gz"}}, channel.ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	defer swapReleaseStore(store)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	rs := findChannel(out.Channels, "releases")
	if rs == nil || !rs.Prerelease {
		t.Fatalf("releases should be reported as a prerelease: %+v", rs)
	}
	if rs.Published {
		t.Error("a prerelease has not reached users: published must stay false")
	}
	if !hasWarningOut(out, output.WarnPrereleaseNotLatest) {
		t.Errorf("status should say it is not latest: %+v", out.Warnings)
	}
	if !hasNextDoOut(out, "wharfy promote --yes") {
		t.Errorf("next should offer the promotion: %+v", out.Next)
	}
}

// TestStatusReleasesPromoted: latest になっていれば普通に published(prerelease の印は消える)。
func TestStatusReleasesPromoted(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases]\n")
	tagScratch(t, root, "v1.2.0")
	chdir(t, root)

	store := channel.NewInMemoryReleaseStore()
	if err := store.Upload(context.Background(), "v1.2.0", "demo 1.2.0",
		[]channel.ReleaseAsset{{Name: "demo_1.2.0_linux_amd64.tar.gz"}}, channel.ReleaseOptions{}); err != nil {
		t.Fatal(err)
	}
	defer swapReleaseStore(store)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	rs := findChannel(out.Channels, "releases")
	if rs == nil || rs.Prerelease || !rs.Published || rs.Version != "1.2.0" {
		t.Fatalf("releases should be published 1.2.0: %+v", rs)
	}
}

func hasWarningOut(out statusOutput, code string) bool {
	for _, w := range out.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestStatusScriptPrereleaseIsNotDrift: prerelease の間、install.sh の実体(latest 経由)は旧版を
// 返す —— それは正常で、drift ではない。drift だと言えば「wharfy publish script」を勧めることに
// なり、publish は昇格前を拒むので、status の言うことが自分で食い違う。
func TestStatusScriptPrereleaseIsNotDrift(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [script]\n")
	tagScratch(t, root, "v1.2.0")
	chdir(t, root)

	// release --yes --prerelease が残す台帳(新版・まだ届いていない)。
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"script": {Version: "1.2.0", Target: "acme/demo release:install.sh", At: "t", Prerelease: true},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
	// 利用者が引く install.sh は昇格まで旧版のまま。
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
	if sc == nil || !sc.Prerelease || sc.Published {
		t.Fatalf("script should read as uploaded-but-not-delivered: %+v", sc)
	}
	if sc.Drift != nil {
		t.Errorf("an unpromoted version is not drift: %+v", sc.Drift)
	}
	if hasNextDoOut(out, "wharfy publish script") {
		t.Error("status must not send you to publish: publish refuses an unpromoted release")
	}
	if !hasNextDoOut(out, "wharfy promote --yes") {
		t.Errorf("the next move is to verify and promote: %+v", out.Next)
	}
}
