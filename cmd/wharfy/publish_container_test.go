package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
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

// loginRec は docker login の呼ばれ方を記録する(実 docker を叩かせない)。
type loginRec struct {
	called           bool
	host, user, argv string
	stdin            string
}

func swapRegistryLogin(rec *loginRec, err error) func() {
	prev := newRegistryLogin
	newRegistryLogin = func() *build.RegistryLogin {
		return &build.RegistryLogin{
			Bin:      "docker",
			LookPath: func(string) (string, error) { return "docker", nil },
			Run: func(_ context.Context, stdin, _ string, args ...string) ([]byte, error) {
				rec.called, rec.stdin, rec.argv = true, stdin, strings.Join(args, " ")
				if len(args) > 3 {
					rec.host, rec.user = args[1], args[3]
				}
				return nil, err
			},
		}
	}
	return func() { newRegistryLogin = prev }
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

// --yes: docker あり → ghcr にログインしてから goreleaser docker pipe(fake)を呼び、
// state に記録、pull を案内。ログインは CI 前提の要(トークンだけ渡せば push が通る)。
func TestPublishContainerApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapDockerAvailable(true)()
	fc := &fakeContainerizer{}
	defer swapContainerizer(fc)()
	rec := &loginRec{}
	defer swapRegistryLogin(rec, nil)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !rec.called || rec.host != "ghcr.io" || rec.user != "acme" || rec.stdin != "tok" {
		t.Errorf("should log in to ghcr as the repo owner: %+v", rec)
	}
	if strings.Contains(rec.argv, "tok") {
		t.Errorf("token must not ride on argv: %q", rec.argv)
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

// CI で container を配ると、push した image の manifest digest にも来歴が付く。
//
// この証言は release では作れない: image を名指せるのは digest だけで、その digest は push を受けた
// レジストリが返して初めて決まる。ここで付けないと、Release のアセットは全部証明されているのに
// image だけが「どこから来たか誰も証明できない配布物」として残る。
func TestPublishContainerAttestsTheImageDigest(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	actionsEnv(t)
	ociRegistry(t, map[string]string{"0.5.0": "sha256:imagedigest", "latest": "sha256:imagedigest"})
	defer swapDockerAvailable(true)()
	defer swapContainerizer(&fakeContainerizer{})()
	defer swapRegistryLogin(&loginRec{}, nil)()
	signer := &recordingAttestSigner{}
	store := &fakeAttestStore{}
	defer swapAttest(signer, store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(store.bundles) != 1 || len(signer.statements) != 1 {
		t.Fatalf("the pushed image must be attested exactly once: %d bundle(s), %d statement(s)",
			len(store.bundles), len(signer.statements))
	}
	// 証言が指すのはレジストリが返した digest。tag ではない —— tag は後から動かせる。
	stmt := string(signer.statements[0])
	if !strings.Contains(stmt, "imagedigest") || !strings.Contains(stmt, "ghcr.io/acme/demo") {
		t.Errorf("the statement must name the image and the digest the registry served: %s", stmt)
	}
	pd := res.Data.(publishData)
	if pd.AttestImage == nil || len(pd.AttestImage.Subjects) != 1 {
		t.Fatalf("publish must report the provenance it attached to the image: %+v", res.Data)
	}
	if !strings.Contains(res.Message, "provenance") {
		t.Errorf("a publish that attested the image should say so: %q", res.Message)
	}
	validateAgainst(t, publishSchemaID, res)
}

// 手元(OIDC 無し)では image に来歴は付かない —— それは異常ではなく前提なので、警告もしない。
func TestPublishContainerOutsideActionsAttestsNothingQuietly(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapDockerAvailable(true)()
	defer swapContainerizer(&fakeContainerizer{})()
	defer swapRegistryLogin(&loginRec{}, nil)()
	store := &fakeAttestStore{}
	defer swapAttest(&recordingAttestSigner{}, store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(store.bundles) != 0 {
		t.Fatal("a laptop has no OIDC identity: nothing may be attested")
	}
	if hasWarning(res, output.WarnAttestUnavailable) {
		t.Error("not being in CI is the premise, not a warning")
	}
}

// ログインに失敗したら、イメージは作らずそこで止まる(401 を build の後まで持ち越さない)。
func TestPublishContainerLoginFailure(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	tagScratch(t, root, "v0.5.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "bad")
	defer swapDockerAvailable(true)()
	fc := &fakeContainerizer{}
	defer swapContainerizer(fc)()
	defer swapRegistryLogin(&loginRec{}, errors.New("denied"))()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"container"})
	if res.OK || len(res.Errors) == 0 {
		t.Fatalf("failed login should fail publish: %+v", res)
	}
	if fc.called {
		t.Error("must not build images when login failed")
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

// swapPrebuiltContainerizer は buildx の呼ばれ方を記録する(実 docker を叩かせない)。
func swapPrebuiltContainerizer(argv *string) func() {
	prev := newPrebuiltContainerizer
	newPrebuiltContainerizer = func() *build.PrebuiltContainerizer {
		return &build.PrebuiltContainerizer{
			Bin:      "docker",
			LookPath: func(string) (string, error) { return "docker", nil },
			Run: func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
				*argv = strings.Join(args, " ")
				return nil, nil
			},
		}
	}
	return func() { newPrebuiltContainerizer = prev }
}

// 一括 publish の BYO(prebuilt)経路も image を push する。byoRelease は archive と Release upload
// しかしないので、ここを飛ばすと「push していないのに published」という嘘の記録が残っていた。
func TestPublishAllPrebuiltPushesContainer(t *testing.T) {
	root := scratchPrebuiltChannels(t, "container")
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapDockerAvailable(true)()
	defer swapReleaseStore(channel.NewInMemoryReleaseStore())()
	defer swapRegistryLogin(&loginRec{}, nil)()
	var argv string
	defer swapPrebuiltContainerizer(&argv)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !strings.Contains(argv, "--push") || !strings.Contains(argv, "ghcr.io/acme/app:0.1.0") {
		t.Errorf("buildx should push the image: %q", argv)
	}
	st, _ := state.Load(root, "app")
	if rec, ok := st.Publish["container"]; !ok || rec.Version != "0.1.0" {
		t.Errorf("container should be recorded after a real push: %+v", st.Publish)
	}
}
