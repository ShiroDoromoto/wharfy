package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// writeConfig は root に wharfy.yaml を置く(verify の対象集合は channels: が決める)。
func writeConfig(t *testing.T, root, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "wharfy.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// recordPublishFor は state に 1 チャネル分の発行記録を足す(verify の前提)。
// 既存の記録は残す ——複数チャネルを配ったプロジェクトを組み立てられるように。
func recordPublishFor(t *testing.T, root, channelName, version, target string) {
	t.Helper()
	st, _ := state.Load(root, "demo")
	if st.Publish == nil {
		st.Publish = map[string]state.PublishRecord{}
	}
	st.Publish[channelName] = state.PublishRecord{Version: version, Target: target, At: "t"}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
}

// recordPublish は homebrew の発行記録を書く(既存 homebrew 検証の前提)。
func recordPublish(t *testing.T, root, version string) {
	t.Helper()
	recordPublishFor(t, root, "homebrew", version, "acme/homebrew-demo")
}

func plantFormula(version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["Formula/demo.rb"] = "class Demo < Formula\n  version \"" + version + "\"\nend\n"
	return s
}

// 未発行 → 何ひとつ検証していないので ok=false。緑で返すと CI が壊れた配布を通す(D-4)。
// dead-end は作らず、channels: にあるチャネルの publish へ導く。
func TestVerifyNothingPublished(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("verifying nothing must not be reported as success: %+v", res)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != output.ErrNothingToVerify {
		t.Fatalf("nothing verified should be nothing_to_verify: %+v", res.Errors)
	}
	if len(res.Next) == 0 || !hasNextDo(res, "wharfy publish homebrew --yes") {
		t.Errorf("verify must guide to publish, not dead-end: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// verify がまだ扱わないチャネル(cask 等)は skipped として checks に載る。
// publish 済みでも「検証した」とは言わない — 検証ゼロなら ok=false のまま。
func TestVerifyUncoveredChannelIsSkippedNotVerified(t *testing.T) {
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\ngithub: acme/demo\nchannels: [cask]\n")
	chdir(t, root)
	recordPublishFor(t, root, "cask", "1.2.0", "acme/homebrew-demo")

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) != 1 || res.Errors[0].Code != output.ErrNothingToVerify {
		t.Fatalf("an uncovered channel is not a verified channel: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "cask" || ck[0].Status != verifyStatusSkipped {
		t.Fatalf("cask should be reported as skipped: %+v", ck)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("wharfy's own gap is not the distributor's warning: %+v", res.Warnings)
	}
	if !hasNextDo(res, "wharfy status") {
		t.Errorf("every channel published but none verifiable: next should be status: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// channels: から外したチャネルの publish 記録が state.json に残っていても検証しない(D-4)。
// 畳んだ tap を検証して緑を返すのが元の壊れ方。記録自体は監査用に消さない。
func TestVerifyIgnoresChannelsRemovedFromConfig(t *testing.T) {
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases]\n")
	chdir(t, root)
	recordPublishFor(t, root, "homebrew", "1.2.0", "acme/homebrew-demo")
	defer swapTapStore(plantFormula("1.2.0"))() // tap は健在。だが channels: に無い

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("a tap outside channels: must not make verify green: %+v", res)
	}
	for _, ck := range checksOf(t, res) {
		if ck.Channel == "homebrew" {
			t.Errorf("homebrew is not in channels: and must not be checked: %+v", ck)
		}
	}
	if hasNextDo(res, "wharfy publish homebrew --yes") {
		t.Errorf("verify must not steer back to a channel the config dropped: %+v", res.Next)
	}
}

// 発行済み＋tap の版が一致 → verified。
func TestVerifyMatch(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(plantFormula("1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("matching version should verify ok: %+v", res)
	}
	if !hasNextDo(res, "wharfy status") {
		t.Errorf("verified next should point to status: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 発行記録あり・tap に formula 無し → verify_failed。
func TestVerifyMissingFormula(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(channel.NewInMemoryTapStore())() // tap 空

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("missing formula should be verify_failed: %+v", res)
	}
}

// 発行記録と tap の版が食い違い → verify_failed。
func TestVerifyVersionMismatch(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(plantFormula("1.1.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("version mismatch should be verify_failed: %+v", res)
	}
}

// plantScoopManifest は bucket に manifest を 1 枚置く(name は manifest のファイル名)。
func plantScoopManifest(name, version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["bucket/"+name+".json"] = "{\n  \"version\": \"" + version + "\"\n}\n"
	return s
}

// scratchScoop は scoop だけを配るプロジェクトを組み立て、その版を発行済みにする。
func scratchScoop(t *testing.T, version string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\ngithub: acme/demo\nchannels: [scoop]\n")
	chdir(t, root)
	recordPublishFor(t, root, "scoop", version, "acme/scoop-demo")
	return root
}

// bucket の manifest の版が期待と一致 → verified。scoop の install は Windows でしか踏めないが、
// bucket は HTTP で読めるので Linux の CI でも壊れたマニフェストを捕まえられる(#1486)。
func TestVerifyScoopMatch(t *testing.T) {
	scratchScoop(t, "1.2.0")
	defer swapTapStore(plantScoopManifest("demo", "1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("matching bucket manifest should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "scoop" || ck[0].Status != verifyStatusOK {
		t.Fatalf("scoop should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// bucket に manifest が無い → verify_failed(publish したはずのものが配られていない)。
func TestVerifyScoopMissingManifest(t *testing.T) {
	scratchScoop(t, "1.2.0")
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing bucket manifest should be verify_failed: %+v", res)
	}
	if !hasNextDo(res, "wharfy publish scoop --yes") {
		t.Errorf("verify must guide to the scoop publish: %+v", res.Next)
	}
}

// bucket の manifest が古い版のまま → verify_failed。
func TestVerifyScoopVersionMismatch(t *testing.T) {
	scratchScoop(t, "1.2.0")
	defer swapTapStore(plantScoopManifest("demo", "1.1.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a stale bucket manifest should be verify_failed: %+v", res)
	}
}

// GUI(bundle)の scoop は bucket/<project>-app.json を所有する。verify が CLI 規約の
// bucket/<project>.json を読みに行くと、健全な配布を「manifest が無い」と誤診する。
func TestVerifyScoopBundleReadsTheAppManifest(t *testing.T) {
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\ngithub: acme/demo\nchannels: [scoop]\nbundle:\n  name: Demo\n  bundles:\n    - { os: windows, arch: amd64, kind: zip, path: dist/Demo-x64.zip }\n")
	chdir(t, root)
	recordPublishFor(t, root, "scoop", "1.2.0", "acme/scoop-demo")
	defer swapTapStore(plantScoopManifest("demo-app", "1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the bundle's app manifest should verify ok: %+v", res)
	}
}

// aptRepoServer は flat repo(<repo>/Packages)に版を 1 つ載せて返す。
func aptRepoServer(t *testing.T, pkg, version string) *httptest.Server {
	t.Helper()
	body := "Package: " + pkg + "\nVersion: " + version + "\nArchitecture: amd64\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Packages" {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rpmRepoServer は repomd→primary を辿れる最小の yum repo を返す。
func rpmRepoServer(t *testing.T, pkg, version string) *httptest.Server {
	t.Helper()
	repomd := `<?xml version="1.0"?><repomd><data type="primary"><location href="repodata/primary.xml"/></data></repomd>`
	primary := `<?xml version="1.0"?><metadata><package><name>` + pkg + `</name><version ver="` + version + `"/></package></metadata>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/repodata/primary.xml":
			_, _ = w.Write([]byte(primary))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scratchLinuxRepo は apt/rpm チャネル 1 本の wharfy.yaml を持つリポを作り、発行記録を書く。
func scratchLinuxRepo(t *testing.T, name, repo, recorded string) string {
	t.Helper()
	return scratchLinuxRepoWith(t, name, repo, recorded, "")
}

// scratchLinuxRepoWith は scratchLinuxRepo に wharfy.yaml の追記(verify: など)を足す。
func scratchLinuxRepoWith(t *testing.T, name, repo, recorded, extraYAML string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: ["+name+"]\n"+name+":\n  repo: "+repo+"\n"+extraYAML)
	recordPublishFor(t, root, name, recorded, repo)
	return root
}

// swapDocker は docker の有無とコンテナ実行を差し替える(実 docker を叩かせない)。
// run は `docker run` だけを受け持つ。イメージの取得(`docker pull`)は既定で成功させ、
// そこを試したいテストだけが swapDockerPullFails で落とす。
func swapDocker(t *testing.T, available bool, run func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	oldAvail, oldRun := dockerAvailable, dockerRun
	dockerAvailable = func() bool { return available }
	if run != nil {
		dockerRun = func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "pull" {
				return nil, nil
			}
			return run(ctx, args...)
		}
	}
	t.Cleanup(func() { dockerAvailable, dockerRun = oldAvail, oldRun })
}

// withInstall は `--install` を立てる(実インストールまで踏む verify)。既定の verify は probe だけ
// なので、コンテナやインストーラを走らせるテストはこれを要る。
func withInstall(t *testing.T) {
	t.Helper()
	old := flagInstall
	flagInstall = true
	t.Cleanup(func() { flagInstall = old })
}

// swapDockerPullFails は `docker pull` が落ちる docker を差し替える(`docker run` は届かない)。
func swapDockerPullFails(t *testing.T, out string) {
	t.Helper()
	swapDockerRaw(t, true, func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "pull" {
			return []byte(out), errors.New("exit status 1")
		}
		t.Fatalf("the container must not run when the image could not be pulled: %v", args)
		return nil, nil
	})
}

// swapDockerRaw は pull を取り繕わずに docker 呼び出しをそのまま渡す(呼び出しの並びを見たいテスト用)。
func swapDockerRaw(t *testing.T, available bool, run func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	oldAvail, oldRun := dockerAvailable, dockerRun
	dockerAvailable, dockerRun = func() bool { return available }, run
	t.Cleanup(func() { dockerAvailable, dockerRun = oldAvail, oldRun })
}

// checksOf は verify の data からチャネル別の結果を取り出す。
func checksOf(t *testing.T, res output.Result) []verifyCheck {
	t.Helper()
	d, ok := res.Data.(verifyData)
	if !ok {
		t.Fatalf("verify data should carry per-channel checks: %+v", res.Data)
	}
	return d.Checks
}

// 既定の verify は repo の版を照合するだけで、コンテナを起こさない(D-4)。CI で毎回叩けるように
// 軽く保つ ——踏んでいない事実は partial と next(--install)で言う。
func TestVerifyAptProbesOnlyByDefault(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		t.Fatal("the default verify must not install: it should stop at the repo probe")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("probing the repo should verify ok: %+v", res)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Fatalf("a probed-only channel is partial, not verified: %+v", ck)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("the default is not a warning; --install is offered as next: %+v", res.Warnings)
	}
	if !hasNextDo(res, "wharfy verify --install") {
		t.Errorf("verify must offer to exercise the install it skipped: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// apt: --install なら repo に版が在るだけでは足りない。コンテナで install して実行するところまで踏む。
func TestVerifyAptInstallsInContainer(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	withInstall(t)
	var args []string
	swapDocker(t, true, func(_ context.Context, a ...string) ([]byte, error) {
		args = a
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("installing from the repo should verify ok: %+v", res)
	}
	if len(args) < 6 || args[0] != "run" || args[2] != "debian:12" {
		t.Fatalf("apt must be exercised in a debian container: %v", args)
	}
	script := args[len(args)-1]
	for _, want := range []string{srv.URL, "apt-get install -y -qq demo", "command -v demo", "demo --version"} {
		if !strings.Contains(script, want) {
			t.Errorf("container script missing %q:\n%s", want, script)
		}
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Errorf("apt check should be verified: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 壊れたパッケージ(依存不足・パス誤り)は install が落ちる → verify_failed。
// 供給側(アップロードの 200)では捕まえられない、消費側だけが踏む失敗。
func TestVerifyAptBrokenPackageFails(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	withInstall(t)
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("demo depends on libfoo; however it is not installable"), errors.New("exit status 100")
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a package that cannot be installed must fail verify: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "not installable") {
		t.Errorf("the container output should be handed back as detail: %+v", res.Errors[0])
	}
	if !hasNextDo(res, "wharfy publish apt --yes") {
		t.Errorf("verify must guide to re-publish: %+v", res.Next)
	}
}

// --install を頼まれたのに docker が無いのは verify の失敗ではない。repo の版までは照合できている
// ので partial とし、踏めなかった事実を warning に残す(何も検証していない nothing_to_verify とは区別する)。
func TestVerifyAptPartialWithoutDocker(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	withInstall(t)
	swapDocker(t, false, func(_ context.Context, _ ...string) ([]byte, error) {
		t.Fatal("docker must not be run when it is unavailable")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("missing docker is not a verify failure: %+v", res)
	}
	if len(res.Warnings) == 0 || res.Warnings[0].Code != output.WarnChannelSkipped {
		t.Fatalf("skipping the install must be visible as a warning: %+v", res.Warnings)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Errorf("apt check should be partial, not verified nor skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// repo の版が記録と食い違うなら、コンテナを起こす前に落とす(入れても確かめたい版ではない)。
func TestVerifyAptVersionMismatchSkipsContainer(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.1.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	withInstall(t)
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		t.Fatal("the container must not run when the repo has the wrong version")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("repo version mismatch should be verify_failed: %+v", res)
	}
}

// rpm は fedora コンテナで dnf を使う(apt の script を使い回さない)。
func TestVerifyRpmInstallsWithDnf(t *testing.T) {
	srv := rpmRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "rpm", srv.URL, "1.2.0"))
	withInstall(t)
	var args []string
	swapDocker(t, true, func(_ context.Context, a ...string) ([]byte, error) {
		args = a
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("installing the rpm should verify ok: %+v", res)
	}
	if len(args) < 6 || args[2] != "fedora:40" {
		t.Fatalf("rpm must be exercised in a fedora container: %v", args)
	}
	script := args[len(args)-1]
	for _, want := range []string{"/etc/yum.repos.d/", "baseurl=" + srv.URL, "dnf install -y -q demo"} {
		if !strings.Contains(script, want) {
			t.Errorf("container script missing %q:\n%s", want, script)
		}
	}
}

// 配る先が debian でないなら、そこで確かめないと検証が的外れになる。verify.images で名指しできる。
func TestVerifyUsesConfiguredImage(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepoWith(t, "apt", srv.URL, "1.2.0", "verify:\n  images:\n    apt: ubuntu:24.04\n"))
	withInstall(t)
	var pulled, ran []string
	swapDockerRaw(t, true, func(_ context.Context, a ...string) ([]byte, error) {
		if a[0] == "pull" {
			pulled = a
			return nil, nil
		}
		ran = a
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the configured image should verify ok: %+v", res)
	}
	if len(pulled) != 2 || pulled[1] != "ubuntu:24.04" {
		t.Errorf("the configured image must be pulled before the run: %v", pulled)
	}
	if len(ran) < 3 || ran[2] != "ubuntu:24.04" {
		t.Fatalf("the container must run on the configured image: %v", ran)
	}
	if !strings.Contains(res.Data.(verifyData).Checks[0].Message, "ubuntu:24.04") {
		t.Errorf("the message should name the image that was exercised: %+v", res.Data)
	}
}

// --version も version も --help も受け付けない CLI がある。起動確認は verify.run で置き換えられる。
func TestVerifyUsesConfiguredRunArgs(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepoWith(t, "apt", srv.URL, "1.2.0", "verify:\n  run: [status, --quiet]\n"))
	withInstall(t)
	var args []string
	swapDocker(t, true, func(_ context.Context, a ...string) ([]byte, error) { args = a; return []byte("ok"), nil })

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the configured launch check should verify ok: %+v", res)
	}
	script := args[len(args)-1]
	if !strings.Contains(script, "demo status --quiet") {
		t.Errorf("the launch check should be the configured command:\n%s", script)
	}
	if strings.Contains(script, "--help") {
		t.Errorf("the default fallback chain should be gone once run is set:\n%s", script)
	}
}

// イメージを引けないのは配布の壊れではない(docker 不在と同じ)。failed ではなく partial に寄せる。
func TestVerifyPartialWhenImageCannotBePulled(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepoWith(t, "apt", srv.URL, "1.2.0", "verify:\n  images:\n    apt: example.invalid/nope:1\n"))
	withInstall(t)
	swapDockerPullFails(t, "Error response from daemon: manifest unknown")

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("an image that cannot be pulled is not a verify failure: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Fatalf("apt check should be partial: %+v", ck)
	}
	if !strings.Contains(ck[0].Message, "example.invalid/nope:1") {
		t.Errorf("the message should name the image that could not be pulled: %+v", ck[0])
	}
	validateAgainst(t, resultSchemaID, res)
}

// 設定の書き間違いは検証環境の都合ではない。コンテナを起こす前に verify_failed で落とす。
func TestVerifyRejectsUnsafeConfiguredImage(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepoWith(t, "apt", srv.URL, "1.2.0", "verify:\n  images:\n    apt: \"debian:12;id\"\n"))
	withInstall(t)
	swapDockerRaw(t, true, func(_ context.Context, a ...string) ([]byte, error) {
		t.Fatalf("docker must not be touched with an unusable image: %v", a)
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("an unusable verify image should be verify_failed: %+v", res)
	}
}

// 設定由来の値をコンテナのシェルへ素通しさせない。イメージ名と起動確認の引数も設定由来なので同じ扱い。
func TestVerifyRejectsUnsafeContainerInputs(t *testing.T) {
	ok := containerVerify{channel: "apt", image: "debian:12", repo: "https://apt.example.com/user/", pkg: "demo", binary: "demo"}
	for _, tc := range []struct {
		name string
		mut  func(*containerVerify)
	}{
		{"non-http repo", func(cv *containerVerify) { cv.repo = "ftp://example.com/deb" }},
		{"shell metachar in repo", func(cv *containerVerify) { cv.repo = "https://example.com/$(id)" }},
		{"space in package name", func(cv *containerVerify) { cv.pkg = "demo pkg" }},
		{"leading dash in binary", func(cv *containerVerify) { cv.binary = "-rf" }},
		{"empty image", func(cv *containerVerify) { cv.image = "" }},
		{"shell metachar in image", func(cv *containerVerify) { cv.image = "debian:12;id" }},
		{"shell metachar in run arg", func(cv *containerVerify) { cv.run = []string{"$(id)"} }},
		{"space in run arg", func(cv *containerVerify) { cv.run = []string{"repo status"} }},
	} {
		cv := ok
		tc.mut(&cv)
		if err := cv.checkShellSafe(); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
	if err := ok.checkShellSafe(); err != nil {
		t.Errorf("a plain https repo should pass: %v", err)
	}
	withRun := ok
	withRun.run = []string{"repo", "--status", "-q"}
	if err := withRun.checkShellSafe(); err != nil {
		t.Errorf("subcommands and flags should pass: %v", err)
	}
}

// ghReleaseServer は tag の Release とアセット本体(name → 中身)を返す最小の GitHub API。
func ghReleaseServer(t *testing.T, tag string, assets map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/demo/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag})
		case r.URL.Path == "/repos/acme/demo/releases/tags/"+tag:
			list := make([]map[string]string, 0, len(assets))
			for name := range assets {
				list = append(list, map[string]string{"name": name, "browser_download_url": srv.URL + "/dl/" + name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": list})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			body, ok := assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scratchReleases は releases チャネル 1 本のリポを作り、発行記録を書く。
func scratchReleases(t *testing.T, recorded string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases]\ngithub: acme/demo\n")
	recordPublishFor(t, root, "releases", recorded, "acme/demo")
	return root
}

// swapReleasesProbe は Release 照合器を httptest に向ける(実 GitHub を叩かせない)。
func swapReleasesProbe(t *testing.T, apiURL string) {
	t.Helper()
	old := newReleasesProbe
	newReleasesProbe = func(owner, repo string) *channel.ReleasesProbe {
		return &channel.ReleasesProbe{Owner: owner, Repo: repo, API: apiURL}
	}
	t.Cleanup(func() { newReleasesProbe = old })
}

// latestJSON は version と資産名から latest.json 本文を組む。
func latestJSON(version string, names ...string) string {
	assets := map[string]string{}
	for i, n := range names {
		assets["k"+string(rune('a'+i))] = "https://github.com/acme/demo/releases/download/v" + version + "/" + n
	}
	b, _ := json.Marshal(map[string]any{"version": version, "assets": assets})
	return string(b)
}

// 既定(--install 無し)は資産名の実在照合まで。名前が揃っても中身は見ていないので verified とは
// 言わず probe で止める(D-4)。
// まっさらな clone(.wharfy が無い＝publish 記録が無い)でも、配った実体から版を決めて確かめる。
// 記録は生成物で gitignore されるので、別ジョブ・別 workflow には渡らない —— そこで全部 skipped に
// 落ちていたら「配ったものが今も入るか」を後から確かめる手段が無い。
func TestVerifyWithoutRecordFallsBackToTheLatestRelease(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases]\ngithub: acme/demo\n") // 記録を書かない
	chdir(t, root)
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the released version should be verifiable without any local record: %+v", res)
	}
	d := res.Data.(verifyData)
	if d.Version != "1.2.0" || d.VersionSource != verifySourceRelease {
		t.Fatalf("the basis should be the latest release, and it should say so: %+v", d)
	}
	if len(d.Checks) != 1 || d.Checks[0].Status == verifyStatusSkipped {
		t.Fatalf("no record must not mean nothing to verify: %+v", d.Checks)
	}
}

// --version は「この版が今も入るか」を名指しで確かめる(記録より強い)。
func TestVerifyVersionFlagOverridesTheRecord(t *testing.T) {
	srv := ghReleaseServer(t, "v1.0.0", map[string]string{
		"latest.json":       latestJSON("1.0.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	chdir(t, scratchReleases(t, "1.2.0")) // 記録は 1.2.0
	swapReleasesProbe(t, srv.URL)
	flagVerifyVersion = "v1.0.0"
	t.Cleanup(func() { flagVerifyVersion = "" })

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the requested version is published, so it should verify: %+v", res)
	}
	d := res.Data.(verifyData)
	if d.Version != "1.0.0" || d.VersionSource != verifySourceRequested {
		t.Fatalf("--version must win over the record: %+v", d)
	}
}

func TestVerifyReleasesAllAssetsPresent(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"install.sh":        "VERSION=\"1.2.0\"",
		"demo_linux.tar.gz": "bin",
	})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a release whose assets all exist should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Fatalf("releases should stop at probe without --install: %+v", ck)
	}
	if !strings.Contains(ck[0].Message, "contents are unchecked") {
		t.Errorf("the probe must say the contents were not checked: %q", ck[0].Message)
	}
	// probe で止めたことは warning にしない(既定どおりに動いただけ)。--install は next で案内する。
	if len(res.Warnings) != 0 {
		t.Errorf("probing by default should not warn: %+v", res.Warnings)
	}
	validateAgainst(t, resultSchemaID, res)
}

// --install: checksums マニフェストの sha256 と実資産が一致する → verified。
func TestVerifyReleasesInstallChecksumsMatch(t *testing.T) {
	assets := map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	}
	assets["demo_1.2.0_checksums.txt"] = checksumsFor(assets, "demo_linux.tar.gz")
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	withInstall(t)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("assets that match their sha256 should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Fatalf("releases check should be verified: %+v", ck)
	}
	if !strings.Contains(ck[0].Message, "match their sha256") {
		t.Errorf("the success must say the sha256 were compared: %q", ck[0].Message)
	}
	validateAgainst(t, resultSchemaID, res)
}

// --install: 資産が checksums の sha256 と食い違う(途中で切れた・差し替えられた)→ verify_failed。
// 名前は在るので、既定の probe では緑のまま通り抜けてしまう壊れ方。
func TestVerifyReleasesInstallChecksumMismatchFails(t *testing.T) {
	assets := map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	}
	assets["demo_1.2.0_checksums.txt"] = checksumsFor(assets, "demo_linux.tar.gz")
	assets["demo_linux.tar.gz"] = "tampered" // マニフェストを書いた後で中身だけ差し替える
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	withInstall(t)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("an asset that does not match its sha256 should be verify_failed: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "demo_linux.tar.gz") {
		t.Errorf("the mismatched asset should be named in the detail: %+v", res.Errors[0])
	}
	validateAgainst(t, resultSchemaID, res)
}

// --install: latest.json しか無い Release は sha256 を持たないので検算できない。緑と呼べば
// 「中身を確かめた」という嘘になるので partial に落とし、warning で言う。
func TestVerifyReleasesInstallWithoutChecksumsIsPartial(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	withInstall(t)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a release without checksums should not fail: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Fatalf("releases should be partial when there is nothing to compare against: %+v", ck)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Message, "no sha256 to compare against") {
		t.Errorf("--install that could not check contents must warn: %+v", res.Warnings)
	}
	validateAgainst(t, resultSchemaID, res)
}

// checksumsFor は assets の本文から GoReleaser 形式の checksums マニフェストを組む。
func checksumsFor(assets map[string]string, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256([]byte(assets[n])), n)
	}
	return b.String()
}

// latest.json に載る資産が Release に無い → verify_failed(利用者はその URL で 404 を踏む)。
func TestVerifyReleasesMissingAssetFails(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz", "demo_windows.zip"),
		"demo_linux.tar.gz": "bin",
	})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing asset should be verify_failed: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "demo_windows.zip") {
		t.Errorf("the missing asset should be named in the detail: %+v", res.Errors[0])
	}
	if !hasNextDo(res, "wharfy release --yes") {
		t.Errorf("missing assets are fixed by re-running release: %+v", res.Next)
	}
}

// latest.json が名乗る版が記録と食い違う → verify_failed(古い版の資産を配っている)。
func TestVerifyReleasesManifestVersionMismatch(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{"latest.json": latestJSON("1.1.0")})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a manifest naming another version should be verify_failed: %+v", res)
	}
}

// tag ごと不在 → verify_failed(到達できないのではなく、配ったはずのものが無い)。
func TestVerifyReleasesReleaseAbsentFails(t *testing.T) {
	srv := ghReleaseServer(t, "v9.9.9", map[string]string{})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("an absent release should be verify_failed, not probe_failed: %+v", res)
	}
}

// latest.json を持たない Release は verify_failed(D-242)。release は必ずこれを上げるので、
// 無いのは配布が壊れている——更新チェックの向き先が 404 になっている。
func TestVerifyReleasesWithoutLatestJSONFails(t *testing.T) {
	srv := ghReleaseServer(t, "v0.9.0", map[string]string{"demo_linux.tar.gz": "bin"})
	chdir(t, scratchReleases(t, "0.9.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a release without %s must be verify_failed: %+v", channel.ManifestLatestJSON, res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusFailed {
		t.Fatalf("releases should be failed: %+v", ck)
	}
	if !strings.Contains(ck[0].Message, channel.ManifestLatestJSON) {
		t.Errorf("the failure must name what is missing: %q", ck[0].Message)
	}
	if !hasNextDo(res, "wharfy release --yes") {
		t.Errorf("a missing manifest is fixed by re-running release: %+v", res.Next)
	}
}

// checksums だけが在る Release も verify_failed。期待集合は checksums から組めてしまうので、
// ここを見張らないと資産照合だけが通って緑になり、latest.json の欠落を配布者が見ない(#1488 の実害)。
func TestVerifyReleasesWithChecksumsButNoLatestJSONFails(t *testing.T) {
	assets := map[string]string{"demo_linux.tar.gz": "bin"}
	assets["demo_0.9.0_checksums.txt"] = checksumsFor(assets, "demo_linux.tar.gz")
	srv := ghReleaseServer(t, "v0.9.0", assets)
	chdir(t, scratchReleases(t, "0.9.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("checksums alone must not make a release without %s green: %+v", channel.ManifestLatestJSON, res)
	}
}

// message は状態ごとにチャネルをまとめる(失敗を先に置き、飛ばした分も隠さない)。
func TestVerifyMessageGroupsByStatus(t *testing.T) {
	got := verifyMessage([]verifyCheck{
		{Channel: "homebrew", Status: verifyStatusOK},
		{Channel: "apt", Status: verifyStatusFailed},
		{Channel: "rpm", Status: verifyStatusSkipped},
	}, false)
	if got != "failed apt; verified homebrew; skipped rpm" {
		t.Errorf("verify message = %q", got)
	}
}

// 検証ゼロのときは「何を飛ばしたか」の前に、確かめられなかったことを名乗る。
func TestVerifyMessageSaysNothingToVerify(t *testing.T) {
	got := verifyMessage([]verifyCheck{{Channel: "releases", Status: verifyStatusSkipped}}, true)
	if got != "nothing to verify: skipped releases" {
		t.Errorf("verify message = %q", got)
	}
	if got := verifyMessage(nil, true); got != "nothing to verify: no channels in wharfy.yaml" {
		t.Errorf("empty channels message = %q", got)
	}
}
