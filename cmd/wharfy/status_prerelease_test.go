package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
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
