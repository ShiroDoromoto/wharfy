package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// verify_install_test.go — script / goinstall の検証と、--install の境目。
//
// 実インストールの末端(sh / go を起こす)は scriptInstall / goinstallInstall を差し替えて締め出す。
// テストがホストへインストールを走らせない、というのはそれ自体が守りたい性質でもある。

// installScriptServer は install.sh を配る最小の Release。version は本文の VERSION= に載る。
func installScriptServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if version == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("#!/bin/sh\nVERSION=\"" + version + "\"\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// moduleProxyServer は tag を持つ(または持たない)最小の module proxy。
func moduleProxyServer(t *testing.T, found bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Version":"v1.2.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// swapScriptInstall は install.sh の実行を差し替える(ホストで走らせない)。
func swapScriptInstall(t *testing.T, run func(ctx context.Context, url, binary string, runArgs []string) ([]byte, error)) {
	t.Helper()
	old := scriptInstall
	scriptInstall = run
	t.Cleanup(func() { scriptInstall = old })
}

// swapGoinstallInstall は go install の実行を差し替える。
func swapGoinstallInstall(t *testing.T, run func(ctx context.Context, path, tag string, runArgs []string) ([]byte, error)) {
	t.Helper()
	old := goinstallInstall
	goinstallInstall = run
	t.Cleanup(func() { goinstallInstall = old })
}

// scratchScript は script チャネル 1 本のリポを作り、発行記録を書く。
func scratchScript(t *testing.T, recorded string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [script]\ngithub: acme/demo\n")
	recordPublishFor(t, root, "script", recorded, "acme/demo release:install.sh")
	return root
}

// 既定は probe だけ: install.sh を取得して VERSION を照合し、走らせはしない。
func TestVerifyScriptProbesOnlyByDefault(t *testing.T) {
	srv := installScriptServer(t, "1.2.0")
	chdir(t, scratchScript(t, "1.2.0"))
	defer swapScriptProbeURL(srv.URL)()
	swapScriptInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		t.Fatal("the default verify must not run install.sh")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a published install.sh at the recorded version should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "script" || ck[0].Status != verifyStatusPartial {
		t.Fatalf("probing without installing is partial: %+v", ck)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("the default is not a warning: %+v", res.Warnings)
	}
	if !hasNextDo(res, "wharfy verify --install") {
		t.Errorf("verify should offer to exercise the install: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 公開 install.sh が別の版を入れる → verify_failed。利用者は記録と違うものを掴む。
func TestVerifyScriptVersionMismatchFails(t *testing.T) {
	srv := installScriptServer(t, "1.1.0")
	chdir(t, scratchScript(t, "1.2.0"))
	defer swapScriptProbeURL(srv.URL)()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("an install.sh installing another version must fail verify: %+v", res)
	}
	if !hasNextDo(res, "wharfy release --yes") {
		t.Errorf("a stale install.sh is fixed by re-running release: %+v", res.Next)
	}
}

// install.sh そのものが Release に無い → verify_failed(記録は在るのに配れていない)。
func TestVerifyScriptMissingFails(t *testing.T) {
	srv := installScriptServer(t, "")
	chdir(t, scratchScript(t, "1.2.0"))
	defer swapScriptProbeURL(srv.URL)()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing install.sh must fail verify: %+v", res)
	}
}

// --install: install.sh を一時 PREFIX へ走らせ、入ったバイナリが起動したら verified。
func TestVerifyScriptInstallRuns(t *testing.T) {
	srv := installScriptServer(t, "1.2.0")
	chdir(t, scratchScript(t, "1.2.0"))
	withInstall(t)
	defer swapScriptProbeURL(srv.URL)()
	var gotURL, gotBinary string
	swapScriptInstall(t, func(_ context.Context, url, binary string, _ []string) ([]byte, error) {
		gotURL, gotBinary = url, binary
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a working installer should verify ok: %+v", res)
	}
	if gotURL != srv.URL || gotBinary != "demo" {
		t.Errorf("install.sh should be run from the probed url with the project binary: %q %q", gotURL, gotBinary)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Errorf("an exercised install is verified, not partial: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// install.sh が落ちる(消えたアセットを掴む等)→ verify_failed。probe だけでは見えない失敗。
func TestVerifyScriptInstallFailureIsVerifyFailed(t *testing.T) {
	srv := installScriptServer(t, "1.2.0")
	chdir(t, scratchScript(t, "1.2.0"))
	withInstall(t)
	defer swapScriptProbeURL(srv.URL)()
	swapScriptInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		return []byte("download failed: 404"), errors.New("exit status 3")
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("an installer that fails must fail verify: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "404") {
		t.Errorf("the installer output should be handed back as detail: %+v", res.Errors[0])
	}
}

// verify.run はコンテナだけの設定ではない。ホストで入れたバイナリの起動確認にも同じ引数を渡す
// ——サブコマンド必須の CLI が、apt では通って script では壊れている、と報告されてはいけない。
func TestVerifyScriptInstallHonoursVerifyRun(t *testing.T) {
	srv := installScriptServer(t, "1.2.0")
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [script]\ngithub: acme/demo\nverify:\n  run: [status, --quiet]\n")
	recordPublishFor(t, root, "script", "1.2.0", "acme/demo release:install.sh")
	chdir(t, root)
	withInstall(t)
	defer swapScriptProbeURL(srv.URL)()
	var gotRun []string
	swapScriptInstall(t, func(_ context.Context, _, _ string, runArgs []string) ([]byte, error) {
		gotRun = runArgs
		return []byte("ok"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("verify should be ok: %+v", res)
	}
	if len(gotRun) != 2 || gotRun[0] != "status" || gotRun[1] != "--quiet" {
		t.Errorf("the launch check on the host must use verify.run: %v", gotRun)
	}
}

// sh が無い環境(Windows 等)で --install を頼まれた → 失敗ではなく partial。何を入れれば踏めるか言う。
func TestVerifyScriptWithoutShellIsPartial(t *testing.T) {
	srv := installScriptServer(t, "1.2.0")
	chdir(t, scratchScript(t, "1.2.0"))
	withInstall(t)
	defer swapScriptProbeURL(srv.URL)()
	swapScriptInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		return nil, errToolMissing
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a missing sh is not a verify failure: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial || !strings.Contains(ck[0].Message, "sh is not available") {
		t.Fatalf("a missing tool should be partial and say so: %+v", ck)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("--install was asked for and not honoured: that is a warning: %+v", res.Warnings)
	}
}

// scratchGoinstall は goinstall チャネル 1 本のリポを作り、タグを打つ(publish 記録は持たない)。
func scratchGoinstall(t *testing.T, tag string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [goinstall]\ngithub: acme/demo\nmain: ./cmd/demo\n")
	tagScratch(t, root, tag)
	return root
}

// goinstall は publish 記録を持たない(何も push しないチャネル)。記録が無くても、タグと module proxy
// で検証できる ——記録の有無で飛ばすと、正しく配れているのに「未発行」と言ってしまう。
func TestVerifyGoinstallProbesWithoutPublishRecord(t *testing.T) {
	proxy := moduleProxyServer(t, true)
	chdir(t, scratchGoinstall(t, "v1.2.0"))
	defer swapGoinstallProxy(proxy.URL)()
	swapGoinstallInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		t.Fatal("the default verify must not run go install")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a tag on the module proxy should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "goinstall" || ck[0].Status != verifyStatusPartial {
		t.Fatalf("goinstall should be probed, not skipped as unpublished: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// proxy が版を知らない → verify_failed。`go install @vX` は解決できず、利用者は入れられない。
func TestVerifyGoinstallNotOnProxyFails(t *testing.T) {
	proxy := moduleProxyServer(t, false)
	chdir(t, scratchGoinstall(t, "v1.2.0"))
	defer swapGoinstallProxy(proxy.URL)()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a tag the proxy cannot resolve must fail verify: %+v", res)
	}
	if !hasNextDo(res, "git push --tags") {
		t.Errorf("an unpushed tag is fixed by pushing it, not by re-publishing: %+v", res.Next)
	}
}

// タグが無ければ `go install` は版を解決しない。検証する対象が無い(失敗ではない)。
func TestVerifyGoinstallWithoutTagIsSkipped(t *testing.T) {
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [goinstall]\ngithub: acme/demo\nmain: ./cmd/demo\n")
	chdir(t, root)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("nothing was verified, so verify must not be green: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusSkipped {
		t.Fatalf("an untagged goinstall is skipped: %+v", ck)
	}
}

// --install: 一時 GOBIN へ go install し、起動したら verified。版の一致は見ない
// (go install は ldflags を通さないので、版を注入している CLI は dev と名乗る)。
func TestVerifyGoinstallInstallRuns(t *testing.T) {
	proxy := moduleProxyServer(t, true)
	chdir(t, scratchGoinstall(t, "v1.2.0"))
	withInstall(t)
	defer swapGoinstallProxy(proxy.URL)()
	var gotPath, gotTag string
	swapGoinstallInstall(t, func(_ context.Context, path, tag string, _ []string) ([]byte, error) {
		gotPath, gotTag = path, tag
		return []byte("demo dev"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("`go install` that runs should verify ok even when the binary says dev: %+v", res)
	}
	if gotPath != "github.com/acme/demo/cmd/demo" || gotTag != "v1.2.0" {
		t.Errorf("go install should target main's package at the tag: %q@%q", gotPath, gotTag)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Errorf("an exercised go install is verified: %+v", ck)
	}
}

// go が無い環境で --install を頼まれた → partial(失敗ではない)。
func TestVerifyGoinstallWithoutGoIsPartial(t *testing.T) {
	proxy := moduleProxyServer(t, true)
	chdir(t, scratchGoinstall(t, "v1.2.0"))
	withInstall(t)
	defer swapGoinstallProxy(proxy.URL)()
	swapGoinstallInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		return nil, errToolMissing
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a missing go toolchain is not a verify failure: %+v", res)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusPartial ||
		!strings.Contains(ck[0].Message, "go is not available") {
		t.Fatalf("a missing tool should be partial and say so: %+v", ck)
	}
}

// go install が落ちる(そのタグでビルドが通らない)→ verify_failed。
func TestVerifyGoinstallBuildFailureIsVerifyFailed(t *testing.T) {
	proxy := moduleProxyServer(t, true)
	chdir(t, scratchGoinstall(t, "v1.2.0"))
	withInstall(t)
	defer swapGoinstallProxy(proxy.URL)()
	swapGoinstallInstall(t, func(context.Context, string, string, []string) ([]byte, error) {
		return []byte("undefined: Foo"), errors.New("exit status 1")
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a module that does not build at the tag must fail verify: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "undefined: Foo") {
		t.Errorf("the go install output should be handed back as detail: %+v", res.Errors[0])
	}
}

// 受け入れ条件(#30): releases / script / goinstall だけで配るプロジェクトが、3 チャネルとも
// 検証されて緑になる。以前は script と goinstall が対象外で、検証ゼロ(ok=false)に落ちていた。
func TestVerifyCoversReleasesScriptAndGoinstall(t *testing.T) {
	gh := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json": latestJSON("1.2.0", "install.sh"),
		"install.sh":  "VERSION=\"1.2.0\"",
	})
	script := installScriptServer(t, "1.2.0")
	proxy := moduleProxyServer(t, true)

	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases, script, goinstall]\ngithub: acme/demo\nmain: ./cmd/demo\n")
	tagScratch(t, root, "v1.2.0")
	recordPublishFor(t, root, "releases", "1.2.0", "acme/demo")
	recordPublishFor(t, root, "script", "1.2.0", "acme/demo release:install.sh")
	chdir(t, root)
	swapReleasesProbe(t, gh.URL)
	defer swapScriptProbeURL(script.URL)()
	defer swapGoinstallProxy(proxy.URL)()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("three covered channels must not report nothing_to_verify: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 3 {
		t.Fatalf("every configured channel should be checked: %+v", ck)
	}
	for _, c := range ck {
		if c.Status == verifyStatusSkipped {
			t.Errorf("%s should be verified or probed, not skipped: %+v", c.Channel, c)
		}
	}
	validateAgainst(t, resultSchemaID, res)
}

// --install が踏むのは「そのホストの利用者が踏むインストーラ」。Windows へ配っている install.ps1 が
// verify から一度も走らない、という穴を塞いだところ。GOOS 依存の分岐は goos を引数に取って締める。
func TestHostScriptInstallerPicksTheInstallerForTheOS(t *testing.T) {
	release := "https://github.com/acme/demo/releases/latest/download/install.sh"
	for _, tc := range []struct {
		goos, shURL, url, name, tool string
	}{
		{"darwin", release, release, "install.sh", "sh"},
		{"linux", release, release, "install.sh", "sh"},
		{
			goos: "windows", shURL: release,
			url:  "https://github.com/acme/demo/releases/latest/download/install.ps1",
			name: "install.ps1", tool: "powershell",
		},
		{ // script.base_url 配下でも install.ps1 は install.sh の隣にある
			goos: "windows", shURL: "https://dl.example.com/demo/install.sh",
			url:  "https://dl.example.com/demo/install.ps1",
			name: "install.ps1", tool: "powershell",
		},
		{ // パスを持たない URL(テストサーバ)でも壊れた URL を組み立てない
			goos: "windows", shURL: "http://127.0.0.1:8080",
			url:  "http://127.0.0.1:8080/install.ps1",
			name: "install.ps1", tool: "powershell",
		},
	} {
		got := hostScriptInstaller(tc.goos, tc.shURL)
		if got.URL != tc.url || got.Name != tc.name || got.Tool != tc.tool {
			t.Errorf("%s should install via %s from %s with %s, got %+v", tc.goos, tc.name, tc.url, tc.tool, got)
		}
	}
}
