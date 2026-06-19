package main

import (
	"context"
	"os/exec"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

const publishSchemaID = "https://wharfy.io/schemas/v1/publish.json"

type fakeArchiver struct {
	arts []build.Artifact
	err  error
}

func (f fakeArchiver) Archives(context.Context, string, string) ([]build.Artifact, error) {
	return f.arts, f.err
}

// fakeArchiver は Releaser も満たす(apply 経路の実リリースを差し替える)。
func (f fakeArchiver) Release(context.Context, string, string) ([]build.Artifact, error) {
	return f.arts, f.err
}

func sampleArchiveArtifacts() []build.Artifact {
	return []build.Artifact{
		{OS: "darwin", Arch: "arm64", Path: ".wharfy/dist/a.tar.gz", SHA256: "11"},
		{OS: "darwin", Arch: "amd64", Path: ".wharfy/dist/b.tar.gz", SHA256: "22"},
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/c.tar.gz", SHA256: "33"},
		{OS: "linux", Arch: "arm64", Path: ".wharfy/dist/d.tar.gz", SHA256: "44"},
		{OS: "windows", Arch: "amd64", Path: ".wharfy/dist/e.zip", SHA256: "55"}, // formula は無視
	}
}

// TestPublishDryRunValidatesSchema: plan プレビューの envelope が publish.json に valid。
func TestPublishDryRunValidatesSchema(t *testing.T) {
	res := output.New("publish", "plan: create Formula/demo.rb", true)
	res.Data = publishData{
		Applied: false,
		Plan: []channel.PlanItem{{
			Channel: "homebrew", Kind: channel.KindOwned,
			OwnedArtifact: "acme/homebrew-demo:Formula/demo.rb",
			Action:        channel.ActionCreate, Diff: "+class Demo < Formula\n",
		}},
		Requires: []requirement{
			{Requirement: "git tag", Met: false, Hint: "git tag vX.Y.Z"},
			{Requirement: "GITHUB_TOKEN", Met: true},
		},
	}
	res.Next = []output.NextDo{{Reason: "apply", Do: "wharfy publish homebrew --yes"}}
	validateAgainst(t, publishSchemaID, res)
}

// TestPublishSkipValidatesSchema: 未対応チャネルの skip も publish.json に valid。
func TestPublishSkipValidatesSchema(t *testing.T) {
	res := output.New("publish", "channel scoop not implemented yet", false)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{{
		Channel: "scoop", Action: channel.ActionSkip, Reason: "slice 1 supports homebrew only",
	}}}
	res.Next = []output.NextDo{{Reason: "use homebrew", Do: "wharfy publish homebrew"}}
	validateAgainst(t, publishSchemaID, res)
}

// TestPublishDryRunWiring: 解決→archive(fake)→Plan までを通し、create plan と diff、--yes 誘導。
func TestPublishDryRunWiring(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "") // tag も token も無い状態を固定
	defer swapArchiver(fakeArchiver{arts: sampleArchiveArtifacts()})()
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	flagYes = false

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || len(pd.Plan) != 1 || pd.Plan[0].Action != channel.ActionCreate {
		t.Fatalf("expected create plan, applied=false: %+v", pd)
	}
	if pd.Plan[0].OwnedArtifact != "acme/homebrew-demo:Formula/demo.rb" {
		t.Errorf("owned_artifact = %q", pd.Plan[0].OwnedArtifact)
	}
	if pd.Plan[0].Diff == "" {
		t.Error("create plan should carry a diff")
	}
	// preview は実 apply の前提(tag/token)を先出しし、両方とも未充足(met=false)で見せる。
	if !requirementUnmet(pd.Requires, "git tag") || !requirementUnmet(pd.Requires, "GITHUB_TOKEN") {
		t.Errorf("requires should list git tag + GITHUB_TOKEN as unmet: %+v", pd.Requires)
	}
	// next: は未充足の前提を解消してから --yes に至る順で出す。
	if !hasNextDo(res, "wharfy publish homebrew --yes") {
		t.Errorf("dry-run should guide to --yes: %+v", res.Next)
	}
	if res.Next[len(res.Next)-1].Do != "wharfy publish homebrew --yes" {
		t.Errorf("--yes should be the last next step after preconditions: %+v", res.Next)
	}
	if store.Commits != 0 {
		t.Errorf("dry-run must not write the tap, commits = %d", store.Commits)
	}
}

func requirementUnmet(reqs []requirement, name string) bool {
	for _, r := range reqs {
		if r.Requirement == name {
			return !r.Met
		}
	}
	return false
}

// TestPublishApplyWiring: --yes で tap に書き、状態に記録する(tag+token あり)。
func TestPublishApplyWiring(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.2.3")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})() // 実リリースを fake 化
	store := channel.NewInMemoryTapStore()
	defer swapTapStore(store)()
	flagYes = true
	defer func() { flagYes = false }()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	pd := res.Data.(publishData)
	if !pd.Applied {
		t.Errorf("expected applied=true: %+v", pd)
	}
	if store.Commits != 1 {
		t.Errorf("apply should write tap once, commits = %d", store.Commits)
	}
	if !hasNextDo(res, "wharfy verify") {
		t.Errorf("apply should guide to verify: %+v", res.Next)
	}
	// archive アップロード(releases)と formula(homebrew)の両方を記録する。
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["homebrew"]; !ok {
		t.Error("homebrew publish should be recorded")
	}
	if _, ok := st.Publish["releases"]; !ok {
		t.Error("releases (archive upload) should be recorded")
	}
}

// TestPublishApplyNeedsTag: tag が無いまま --yes は tag_missing で停止。
func TestPublishApplyNeedsTag(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaser(fakeArchiver{arts: sampleArchiveArtifacts()})()
	defer swapTapStore(channel.NewInMemoryTapStore())()
	flagYes = true
	defer func() { flagYes = false }()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrTagMissing {
		t.Fatalf("expected tag_missing, got %+v", res)
	}
}

// --- helpers ---

func swapArchiver(a build.Archiver) func() {
	prev := newArchiver
	newArchiver = func(string) build.Archiver { return a }
	return func() { newArchiver = prev }
}

func swapReleaser(r build.Releaser) func() {
	prev := newReleaser
	newReleaser = func(string) build.Releaser { return r }
	return func() { newReleaser = prev }
}

func swapTapStore(s channel.TapStore) func() {
	prev := newTapStore
	newTapStore = func(string, string, string) channel.TapStore { return s }
	return func() { newTapStore = prev }
}

// tagScratch は HEAD にコミットを作り tag を付ける(gitCurrentTag が拾えるように)。
func tagScratch(t *testing.T, root, tag string) {
	t.Helper()
	cmds := [][]string{
		{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A"},
		{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init"},
		{"-C", root, "tag", tag},
	}
	for _, a := range cmds {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
}
