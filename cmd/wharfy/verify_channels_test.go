package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// verify_channels_test.go — cask / container / aur / winget を verify が実体で確かめることの検証(#1520)。
// これらは以前 default に落ちて skip され、壊れて配っても CI は緑のまま通っていた。

// noReleases は Release 照合器を「Release の無い repo」に向ける(実 GitHub を叩かせない)。
// 記録が在るチャネルでも fallback は必ず 1 度引かれるので、どのテストでも要る。
func noReleases(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	swapReleasesProbe(t, srv.URL)
}

// scratchChannel は 1 チャネルだけを配るプロジェクトを組み立て、その版を発行済みにする。
func scratchChannel(t *testing.T, name, version, target string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\ngithub: acme/demo\nchannels: ["+name+"]\n")
	chdir(t, root)
	recordPublishFor(t, root, name, version, target)
	noReleases(t)
	return root
}

// plantCask は tap に cask を 1 枚置く(既定 token は <project>-app)。
func plantCask(version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["Casks/demo-app.rb"] = "cask \"demo-app\" do\n  version \"" + version + "\"\nend\n"
	return s
}

// tap の cask の版が期待と一致 → verified。cask の install は macOS でしか踏めないが、tap は HTTP で
// 読めるので Linux の CI でも壊れた cask を捕まえられる。
func TestVerifyCaskMatch(t *testing.T) {
	scratchChannel(t, "cask", "1.2.0", "acme/homebrew-demo")
	defer swapTapStore(plantCask("1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a matching cask should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "cask" || ck[0].Status != verifyStatusOK {
		t.Fatalf("cask should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// tap に cask が無い → verify_failed(publish したはずのものが配られていない)。
func TestVerifyCaskMissing(t *testing.T) {
	scratchChannel(t, "cask", "1.2.0", "acme/homebrew-demo")
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing cask should be verify_failed: %+v", res)
	}
	if !hasNextDo(res, "wharfy publish cask --yes") {
		t.Errorf("verify must guide to the cask publish: %+v", res.Next)
	}
}

// tap の cask が古い版のまま → verify_failed。
func TestVerifyCaskVersionMismatch(t *testing.T) {
	scratchChannel(t, "cask", "1.2.0", "acme/homebrew-demo")
	defer swapTapStore(plantCask("1.1.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a stale cask should be verify_failed: %+v", res)
	}
}

// ociRegistry は tag→digest を返すレジストリ(在る tag だけ 200 + Docker-Content-Digest)。
func ociRegistry(t *testing.T, tags map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		for tag, digest := range tags {
			if strings.HasSuffix(r.URL.Path, "/manifests/"+tag) {
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(swapOCIProbeBase(srv.URL))
}

// 版の tag が在り、:latest が同じ image を指す → verified。
func TestVerifyContainerTagPresent(t *testing.T) {
	scratchChannel(t, "container", "1.2.0", "ghcr.io/acme/demo")
	ociRegistry(t, map[string]string{"1.2.0": "sha256:aaa", "latest": "sha256:aaa"})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("an existing image tag should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "container" || ck[0].Status != verifyStatusOK {
		t.Fatalf("container should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// tag が消えている(または push できていない)→ verify_failed。`docker pull` が 404 になる。
func TestVerifyContainerTagMissing(t *testing.T) {
	scratchChannel(t, "container", "1.2.0", "ghcr.io/acme/demo")
	ociRegistry(t, nil)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing image tag should be verify_failed: %+v", res)
	}
	if !hasNextDo(res, "wharfy publish container --yes") {
		t.Errorf("verify must guide to the container publish: %+v", res.Next)
	}
}

// :latest が古い image を指したまま → verify_failed。tag を省いた `docker pull <image>` を踏む
// 利用者は古い版を掴むのに、版の tag だけを見ていると緑で通ってしまう(#1532)。
func TestVerifyContainerLatestPointsAtAnotherImage(t *testing.T) {
	scratchChannel(t, "container", "1.2.0", "ghcr.io/acme/demo")
	ociRegistry(t, map[string]string{"1.2.0": "sha256:new", "latest": "sha256:old"})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a latest tag left on the old image should be verify_failed: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Detail, "sha256:old") {
		t.Errorf("the detail should show what latest actually points at: %+v", res.Errors[0])
	}
}

// :latest がそもそも無い → verify_failed(`docker pull <image>` が 404)。
func TestVerifyContainerLatestMissing(t *testing.T) {
	scratchChannel(t, "container", "1.2.0", "ghcr.io/acme/demo")
	ociRegistry(t, map[string]string{"1.2.0": "sha256:aaa"})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a missing latest tag should be verify_failed: %+v", res)
	}
}

// --version で古い版を名指した検証では :latest を照合しない —— :latest がその版を指していないのが
// 正しい姿で、赤くすると嘘になる。
func TestVerifyContainerRequestedVersionDoesNotCompareLatest(t *testing.T) {
	scratchChannel(t, "container", "1.2.0", "ghcr.io/acme/demo")
	ociRegistry(t, map[string]string{"1.1.0": "sha256:old", "latest": "sha256:new"})
	flagVerifyVersion = "v1.1.0"
	t.Cleanup(func() { flagVerifyVersion = "" })

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a named older version is published; latest is not its business: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Fatalf("the named version exists, so it verifies: %+v", ck)
	}
}

// aurRPC は AUR RPC を模す(version が空なら「パッケージが無い」)。
func aurRPC(t *testing.T, version string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version == "" || !strings.HasSuffix(r.URL.Path, "/rpc/v5/info/demo-bin") {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"Version":"` + version + `"}]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(swapAurRPCBase(srv.URL))
}

// AUR の pkgver が期待と一致 → verified。pkgrel(-1)は AUR 側の再ビルド番号なので照合に混ぜない。
func TestVerifyAurMatch(t *testing.T) {
	scratchChannel(t, "aur", "1.2.0", "demo-bin")
	aurRPC(t, "1.2.0-1")

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a matching AUR package should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "aur" || ck[0].Status != verifyStatusOK {
		t.Fatalf("aur should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// AUR に古い版しか無い → verify_failed(`yay -S` は古い版を入れる)。
func TestVerifyAurVersionMismatch(t *testing.T) {
	scratchChannel(t, "aur", "1.2.0", "demo-bin")
	aurRPC(t, "1.1.0-1")

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a stale AUR package should be verify_failed: %+v", res)
	}
	if !hasNextDo(res, "wharfy publish aur --yes") {
		t.Errorf("verify must guide to the aur publish: %+v", res.Next)
	}
}

// plantWingetManifest は中央リポジトリに版の manifest を 1 枚置く。
func plantWingetManifest(identifier, version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	dir := channel.WingetInput{Identifier: identifier, Version: version}.ManifestDir()
	s.Files[dir+identifier+".yaml"] = "PackageIdentifier: " + identifier + "\nPackageVersion: " + version + "\n"
	return s
}

// 中央リポジトリに manifest が在る＝Microsoft が merge した＝`winget install` が届く → verified。
func TestVerifyWingetMergedUpstream(t *testing.T) {
	scratchChannel(t, "winget", "1.2.0", "acme.demo")
	defer swapTapStore(plantWingetManifest("acme.demo", "1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a merged winget manifest should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "winget" || ck[0].Status != verifyStatusOK {
		t.Fatalf("winget should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// plantCoreFormula は上流 homebrew-core に formula を 1 枚置く。core の formula は version stanza を
// 持たず、Homebrew が url のタグから版を推す —— verify もそこから読む。
func plantCoreFormula(version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["Formula/d/demo.rb"] = "class Demo < Formula\n" +
		"  url \"https://github.com/acme/demo/archive/refs/tags/v" + version + ".tar.gz\"\n" +
		"  sha256 \"abc\"\nend\n"
	return s
}

// 上流 core の formula が期待の版 → verified(`brew install demo` がその版を入れる)。
func TestVerifyHomebrewCoreMergedUpstream(t *testing.T) {
	scratchChannel(t, "homebrew-core", "1.2.0", "Homebrew/homebrew-core")
	defer swapTapStore(plantCoreFormula("1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a merged core formula should verify ok: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "homebrew-core" || ck[0].Status != verifyStatusOK {
		t.Fatalf("homebrew-core should be verified, not skipped: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 上流 core が古い版のまま＝申請がまだ merge されていない → partial + gated の警告(failed ではない)。
// 自前 tap が最新でも、core が古ければ `brew install demo` は古い版を入れる —— そこを黙らない。
func TestVerifyHomebrewCoreStaleUpstreamIsPending(t *testing.T) {
	scratchChannel(t, "homebrew-core", "1.2.0", "Homebrew/homebrew-core")
	defer swapTapStore(plantCoreFormula("1.1.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a pending gated submission is not a broken distribution: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial || !strings.Contains(ck[0].Message, "1.1.0") {
		t.Fatalf("verify should say which version core still serves: %+v", ck)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != output.WarnGatedPending {
		t.Fatalf("verify must say the version has not reached users: %+v", res.Warnings)
	}
	validateAgainst(t, resultSchemaID, res)
}

// core に formula がまだ無い(初回申請が審査中 / 未提出)→ partial + gated の警告。
func TestVerifyHomebrewCoreNotUpstreamYet(t *testing.T) {
	scratchChannel(t, "homebrew-core", "1.2.0", "Homebrew/homebrew-core")
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("an unmerged submission is not a broken distribution: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Status != verifyStatusPartial {
		t.Fatalf("a formula not yet in core should be partial: %+v", ck)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != output.WarnGatedPending {
		t.Fatalf("verify must say the version has not reached users: %+v", res.Warnings)
	}
}

// 中央にまだ載っていない winget は partial + gated の警告。failed にはしない —— merge するのは
// Microsoft で、配布者に打てる手は待つことだけ(D-243)。--install を勧めるのも誤り(踏める install が無い)。
func TestVerifyWingetPendingIsPartialNotFailed(t *testing.T) {
	scratchChannel(t, "winget", "1.2.0", "acme.demo")
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a pending gated submission is not a broken distribution: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "winget" || ck[0].Status != verifyStatusPartial {
		t.Fatalf("a winget version not yet upstream should be partial: %+v", ck)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != output.WarnGatedPending {
		t.Fatalf("verify must say the version has not reached users: %+v", res.Warnings)
	}
	if hasNextDo(res, "wharfy verify --install") {
		t.Errorf("there is nothing to install for a gated channel that is not upstream yet: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}
