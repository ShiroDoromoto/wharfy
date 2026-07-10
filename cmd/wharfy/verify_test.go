package main

import (
	"context"
	"errors"
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

// recordPublish は state に homebrew 発行記録を書く(verify の前提)。
func recordPublish(t *testing.T, root, version string) {
	t.Helper()
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"homebrew": {Version: version, Target: "acme/homebrew-demo", At: "t"},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
}

func plantFormula(version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["Formula/demo.rb"] = "class Demo < Formula\n  version \"" + version + "\"\nend\n"
	return s
}

// 未発行 → 確認対象なしを正直に返し、publish へ導く(空 next の dead-end を作らない)。
func TestVerifyNothingPublished(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("nothing-to-verify is not a failure: %+v", res)
	}
	if len(res.Next) == 0 || !hasNextDo(res, "wharfy publish homebrew --yes") {
		t.Errorf("verify must guide to publish, not dead-end: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
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
	root := scratchModule(t)
	yaml := "project: demo\nchannels: [" + name + "]\n" + name + ":\n  repo: " + repo + "\n"
	if err := os.WriteFile(filepath.Join(root, "wharfy.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{name: {Version: recorded, Target: repo, At: "t"}}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
	return root
}

// swapDocker は docker の有無とコンテナ実行を差し替える(実 docker を叩かせない)。
func swapDocker(t *testing.T, available bool, run func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	oldAvail, oldRun := dockerAvailable, dockerRun
	dockerAvailable = func() bool { return available }
	if run != nil {
		dockerRun = run
	}
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

// apt: repo に版が在るだけでは足りない。コンテナで install して実行するところまで踏む。
func TestVerifyAptInstallsInContainer(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
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

// docker 不在は verify の失敗ではない。skip したうえで、飛ばした事実を warning に残す。
func TestVerifyAptSkippedWithoutDocker(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
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
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusSkipped {
		t.Errorf("apt check should be skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// repo の版が記録と食い違うなら、コンテナを起こす前に落とす(入れても確かめたい版ではない)。
func TestVerifyAptVersionMismatchSkipsContainer(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.1.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
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

// 設定由来の値をコンテナのシェルへ素通しさせない。
func TestVerifyRejectsUnsafeContainerInputs(t *testing.T) {
	for _, tc := range []struct{ name, repo, pkg, binary string }{
		{"non-http repo", "ftp://example.com/deb", "demo", "demo"},
		{"shell metachar in repo", "https://example.com/$(id)", "demo", "demo"},
		{"space in package name", "https://example.com/deb", "demo pkg", "demo"},
		{"leading dash in binary", "https://example.com/deb", "demo", "-rf"},
	} {
		if err := checkShellSafe(tc.repo, tc.pkg, tc.binary); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
	if err := checkShellSafe("https://apt.example.com/user/", "demo", "demo"); err != nil {
		t.Errorf("a plain https repo should pass: %v", err)
	}
}

// message は状態ごとにチャネルをまとめる(失敗を先に置き、飛ばした分も隠さない)。
func TestVerifyMessageGroupsByStatus(t *testing.T) {
	got := verifyMessage([]verifyCheck{
		{Channel: "homebrew", Status: verifyStatusOK},
		{Channel: "apt", Status: verifyStatusFailed},
		{Channel: "rpm", Status: verifyStatusSkipped},
	})
	if got != "failed apt; verified homebrew; skipped rpm" {
		t.Errorf("verify message = %q", got)
	}
}
