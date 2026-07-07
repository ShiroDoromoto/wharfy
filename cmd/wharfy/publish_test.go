package main

import (
	"context"
	"fmt"
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

// sampleVersion は sampleArchiveArtifacts が使う既定版。apply 経路のテストはこの版で tag する
// (publish の sha 自己検査 #10 が URL の資産名で突き合わせるため、artifact 名と tag 版を揃える)。
const sampleVersion = "1.0.0"

// sampleArchiveArtifacts は release が上げた成果物の疑似セット。Path basename を実アップロード名
// (<project>_<version>_<os>_<arch>.<ext>)に一致させる — #10 の自己検査が URL の資産名で突き合わせるため、
// 非現実的な名前(旧 a.tar.gz 等)だと検査に弾かれる。版を跨ぐ apply テストは namedArtifacts を使う。
func sampleArchiveArtifacts() []build.Artifact { return namedArtifacts("demo", sampleVersion) }

// namedArtifacts は project/version の実アップロード名を持つ疑似成果物を返す。
func namedArtifacts(project, version string) []build.Artifact {
	mk := func(os, arch, ext, sha string) build.Artifact {
		return build.Artifact{
			OS: os, Arch: arch, SHA256: sha,
			Path: fmt.Sprintf(".wharfy/dist/%s_%s_%s_%s.%s", project, version, os, arch, ext),
		}
	}
	return []build.Artifact{
		mk("darwin", "arm64", "tar.gz", "11"),
		mk("darwin", "amd64", "tar.gz", "22"),
		mk("linux", "amd64", "tar.gz", "33"),
		mk("linux", "arm64", "tar.gz", "44"),
		mk("windows", "amd64", "zip", "55"), // formula は無視 / scoop・winget が使う
	}
}

// TestFormulaArchivesExcludesBundles: prebuilt CLI + bundle(dmg/deb)併用時、formula は
// CLI archive(Kind 空)だけを引き、bundle を混ぜない。混ぜると dmg(OS=darwin)が darwin/arm64 の
// tarball 参照に dmg の sha を載せ、cask と同一 sha を記録して brew が全 artifact を弾く(request.md)。
func TestFormulaArchivesExcludesBundles(t *testing.T) {
	archs := []build.Artifact{
		{OS: "darwin", Arch: "arm64", Path: ".wharfy/dist/cli_darwin_arm64.tar.gz", SHA256: "cli-darwin-arm64"},
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/cli_linux_amd64.tar.gz", SHA256: "cli-linux-amd64"},
		{OS: "darwin", Arch: "arm64", Kind: "dmg", Path: ".wharfy/dist/app.dmg", SHA256: "dmg-darwin-arm64"},
		{OS: "linux", Arch: "amd64", Kind: "deb", Path: ".wharfy/dist/app.deb", SHA256: "deb-linux-amd64"},
	}
	got := formulaArchives(archs, "acme", "widget-dist", "widget", "0.1.1")
	if len(got) != 2 {
		t.Fatalf("formula should keep only the 2 CLI archives, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.OS == "darwin" && r.Arch == "arm64" && r.SHA256 != "cli-darwin-arm64" {
			t.Errorf("darwin/arm64 must carry the CLI tarball sha, not a bundle sha: %+v", r)
		}
		if r.SHA256 == "dmg-darwin-arm64" || r.SHA256 == "deb-linux-amd64" {
			t.Errorf("bundle sha leaked into formula archive: %+v", r)
		}
	}
}

// TestAurSourcesExcludesBundles: AUR も linux bundle(deb/rpm/appimage)を source archive に混ぜない。
func TestAurSourcesExcludesBundles(t *testing.T) {
	archs := []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/cli_linux_amd64.tar.gz", SHA256: "cli-linux-amd64"},
		{OS: "linux", Arch: "amd64", Kind: "deb", Path: ".wharfy/dist/app.deb", SHA256: "deb-linux-amd64"},
	}
	got := aurSources(archs, "acme", "widget-dist", "widget", "0.1.1")
	if len(got) != 1 || got[0].SHA256 != "cli-linux-amd64" {
		t.Fatalf("aur should keep only the CLI archive: %+v", got)
	}
}

// TestVerifyManifestChecksums: manifest の (URL→sha) を実 artifact の sha と突き合わせ、
// 一致は nil、不一致/URL 資産名の不在は error(#10 の自己検査 = #9 の多層防御)。
func TestVerifyManifestChecksums(t *testing.T) {
	archs := []build.Artifact{
		{OS: "darwin", Arch: "arm64", Path: ".wharfy/dist/widget_0.1.1_darwin_arm64.tar.gz", SHA256: "cli-arm64"},
		{OS: "darwin", Arch: "arm64", Kind: "dmg", Path: ".wharfy/dist/widget-app.dmg", SHA256: "dmg-arm64"},
	}
	base := "https://github.com/acme/widget-dist/releases/download/v0.1.1/"
	hb := func(sha string) *channel.Homebrew {
		return &channel.Homebrew{Input: channel.FormulaInput{Archives: []channel.ArchiveRef{
			{OS: "darwin", Arch: "arm64", URL: base + "widget_0.1.1_darwin_arm64.tar.gz", SHA256: sha},
		}}}
	}
	// 正: URL の指す tarball と記録 sha が一致。
	if err := verifyManifestChecksums(hb("cli-arm64"), archs); err != nil {
		t.Errorf("matching sha must pass: %v", err)
	}
	// 誤: #9 の型 — CLI tarball の URL に dmg の sha を載せる → fail。
	if err := verifyManifestChecksums(hb("dmg-arm64"), archs); err == nil {
		t.Error("bundle sha under a CLI tarball URL must fail")
	}
	// 誤: URL の指す資産が upload 済み成果物に無い(404 誘発)→ fail。
	miss := &channel.Homebrew{Input: channel.FormulaInput{Archives: []channel.ArchiveRef{
		{OS: "linux", Arch: "amd64", URL: base + "widget_0.1.1_linux_amd64.tar.gz", SHA256: "x"},
	}}}
	if err := verifyManifestChecksums(miss, archs); err == nil {
		t.Error("a URL with no matching uploaded artifact must fail")
	}
	// 対象外: ChecksumSource 未実装の Publisher は素通り(nil)。
	if err := verifyManifestChecksums(&notChecksumSource{}, archs); err != nil {
		t.Errorf("non-ChecksumSource publisher must be skipped: %v", err)
	}
}

// notChecksumSource は ChecksumSource を実装しない Publisher(検査対象外の確認用)。
type notChecksumSource struct{}

func (notChecksumSource) Name() string                                  { return "x" }
func (notChecksumSource) Kind() string                                  { return channel.KindOwned }
func (notChecksumSource) Plan(context.Context) (channel.PlanItem, error) { return channel.PlanItem{}, nil }
func (notChecksumSource) Publish(context.Context) (channel.PlanItem, channel.PubResult, error) {
	return channel.PlanItem{}, channel.PubResult{}, nil
}
func (notChecksumSource) Probe(context.Context) (channel.RemoteState, error) {
	return channel.RemoteState{}, nil
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
	tagScratch(t, root, "v"+sampleVersion) // artifact 名と版を揃える(#10 の sha 自己検査)
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
