package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

type fakeContainerizer struct {
	called bool
	err    error
}

func (f *fakeContainerizer) Containers(context.Context, string, string) ([]build.Artifact, error) {
	f.called = true
	return nil, f.err
}

func swapContainerizer(c build.Containerizer) func() {
	prev := newContainerizer
	newContainerizer = func(string) build.Containerizer { return c }
	return func() { newContainerizer = prev }
}

func swapDockerAvailable(v bool) func() {
	prev := dockerAvailable
	dockerAvailable = func() bool { return v }
	return func() { dockerAvailable = prev }
}

// dry-run: image とタグを見せ、docker を前提条件に出す。
func TestPublishContainerDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	defer swapDockerAvailable(false)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || pd.Plan[0].OwnedArtifact != "ghcr.io/acme/demo" {
		t.Errorf("plan target wrong: %+v", pd.Plan[0])
	}
	if !strings.Contains(pd.Plan[0].Diff, "ghcr.io/acme/demo:0.0.0-amd64") {
		t.Errorf("diff should list per-arch tags: %q", pd.Plan[0].Diff)
	}
	if !requirementUnmet(pd.Requires, "docker") || !requirementUnmet(pd.Requires, "GITHUB_TOKEN") {
		t.Errorf("docker + GITHUB_TOKEN should be unmet: %+v", pd.Requires)
	}
}

// --yes: docker あり → goreleaser docker pipe(fake)を呼び、state に記録、pull を案内。
func TestPublishContainerApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapDockerAvailable(true)()
	fc := &fakeContainerizer{}
	defer swapContainerizer(fc)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !fc.called {
		t.Error("containerizer should run on apply")
	}
	if !hasNextDo(res, "docker pull ghcr.io/acme/demo:0.5.0") {
		t.Errorf("should advise docker pull: %+v", res.Next)
	}
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["container"]; !ok {
		t.Error("container publish should be recorded")
	}
}

// --yes: docker 無し → builder_unavailable で停止(イメージは作らない)。
func TestPublishContainerNoDocker(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapDockerAvailable(false)()
	fc := &fakeContainerizer{}
	defer swapContainerizer(fc)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrBuilderUnavailable {
		t.Fatalf("no docker should be builder_unavailable: %+v", res)
	}
	if fc.called {
		t.Error("must not build images without docker")
	}
}
