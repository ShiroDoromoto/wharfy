package main

// verify_attest_test.go — 「証明したつもり」を verify が捕まえることの検証。
//
// 来歴は付けただけでは意味を持たない。ここで確かめるのは、付いていない・一部にしか付いていない・
// 付いているのに検算できない、の 3 つが**それぞれ違う言い方で**出ること。全部を skip や緑に畳むと、
// リリースの CI は壊れた証明をそのまま通す。

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/attest"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// attestedRelease は「Release の全成果物に、検算できる来歴が付いている」ふりをする末端
// (Fetcher と BundleVerifier の両方)。digest ごとに bundle を 1 つ返し、検算は必ず通す。
type attestedRelease struct {
	// missing は来歴が預けられていない資産の digest。
	missing map[string]bool
	// broken は来歴は在るが検算できない資産の digest。
	broken map[string]bool
	// asked は実際に引き当てた digest(引いていない資産が無いことの確認に使う)。
	asked map[string]bool
}

func (a attestedRelease) Bundles(_ context.Context, sha256 string) ([][]byte, error) {
	if a.asked != nil {
		a.asked[sha256] = true
	}
	if a.missing[sha256] {
		return nil, nil
	}
	return [][]byte{[]byte(`{"bundle":"` + sha256 + `"}`)}, nil
}

func (a attestedRelease) VerifyBundle(_ context.Context, _ []byte, sha256 string, _ attest.Identity) error {
	if a.broken[sha256] {
		return errString("the certificate was not issued to this repository's workflow")
	}
	return nil
}

// swapAttest は来歴の末端(取り口と検算器)を差し替える。
func swapAttestVerify(t *testing.T, fake attestedRelease) {
	t.Helper()
	oldF, oldV := newAttestFetcher, newAttestVerifier
	newAttestFetcher = func(_, _, _ string) attest.Fetcher { return fake }
	newAttestVerifier = func() attest.BundleVerifier { return fake }
	t.Cleanup(func() { newAttestFetcher, newAttestVerifier = oldF, oldV })
}

// releaseAssets は成果物 2 本と、来歴を付けない資産(install.sh / latest.json / checksums)を持つ Release。
func releaseAssets() map[string]string {
	assets := map[string]string{
		"latest.json":        latestJSON("1.2.0", "demo_linux.tar.gz", "demo_darwin.tar.gz"),
		"install.sh":         "VERSION=\"1.2.0\"",
		"demo_linux.tar.gz":  "linux bin",
		"demo_darwin.tar.gz": "darwin bin",
	}
	assets["demo_1.2.0_checksums.txt"] = checksumsFor(assets, "demo_linux.tar.gz", "demo_darwin.tar.gz")
	return assets
}

// 付いていて、検算も通る —— verify は緑で、来歴を確かめたと言い切る。
func TestVerifyAttestVerifiesEveryBuildArtifact(t *testing.T) {
	assets := releaseAssets()
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	asked := map[string]bool{}
	swapAttestVerify(t, attestedRelease{asked: asked})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a fully attested release should verify ok: %+v", res)
	}
	ck := checkFor(t, res, attestCheckName)
	if ck.Status != verifyStatusOK {
		t.Fatalf("provenance that verifies should be verified, not %s: %+v", ck.Status, ck)
	}
	if !strings.Contains(ck.Message, "all 2 build artifacts") {
		t.Errorf("the check should say how many artifacts it proved: %q", ck.Message)
	}
	// 引くのはビルド成果物だけ。install.sh / latest.json / checksums は attest の subject ではないので、
	// ここを引きにいくと「付いていない」を無いはずの欠落として数えることになる。
	for _, name := range []string{"demo_linux.tar.gz", "demo_darwin.tar.gz"} {
		if !asked[sha256Hex(assets[name])] {
			t.Errorf("%s is a build artifact: its provenance must be looked up", name)
		}
	}
	for _, name := range []string{"install.sh", "latest.json", "demo_1.2.0_checksums.txt"} {
		if asked[sha256Hex(assets[name])] {
			t.Errorf("%s is not a build output, so it is not attested — verify must not demand provenance for it", name)
		}
	}
	validateAgainst(t, resultSchemaID, res)
}

// 一部にしか付いていない —— これが「付けたつもり」の正体。どちらの資産を取るかで証明の有無が変わる
// 配布を緑で通さない(failed)。
func TestVerifyAttestFailsWhenOnlySomeArtifactsAreAttested(t *testing.T) {
	assets := releaseAssets()
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	swapAttestVerify(t, attestedRelease{missing: map[string]bool{sha256Hex(assets["demo_darwin.tar.gz"]): true}})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("a partly attested release must not be green: %+v", res)
	}
	if ck := checkFor(t, res, attestCheckName); ck.Status != verifyStatusFailed {
		t.Fatalf("a gap in provenance is a failure, not a partial: %+v", ck)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrAttestUnverified {
		t.Fatalf("the problem should be attest_unverified: %+v", res.Errors)
	}
	if !strings.Contains(res.Errors[0].Detail, "demo_darwin.tar.gz") {
		t.Errorf("the artifact without provenance must be named: %+v", res.Errors[0])
	}
	validateAgainst(t, resultSchemaID, res)
}

// 付いているのに検算できない(別の workflow が署名した・ログに載っていない)。証明が在ることと、
// 誰かが検算できることは別 —— 在るだけで緑にすると、この壊れ方は永遠に見つからない。
func TestVerifyAttestFailsWhenProvenanceDoesNotVerify(t *testing.T) {
	assets := releaseAssets()
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	swapAttestVerify(t, attestedRelease{broken: map[string]bool{sha256Hex(assets["demo_linux.tar.gz"]): true}})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("provenance that does not verify must not be green: %+v", res)
	}
	if ck := checkFor(t, res, attestCheckName); ck.Status != verifyStatusFailed {
		t.Fatalf("unverifiable provenance is a failure: %+v", ck)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrAttestUnverified {
		t.Fatalf("the problem should be attest_unverified: %+v", res.Errors)
	}
	if !strings.Contains(res.Errors[0].Detail, "this repository's workflow") {
		t.Errorf("the detail should carry why it did not verify: %+v", res.Errors[0])
	}
}

// 1 つも付いていない —— 手元から配った、permissions を書いていない、どちらも「まだ付けていない」。
// 配布者に打てる手は在る(workflow の permissions)ので、赤くはせず警告で言う。
func TestVerifyAttestIsPartialWhenNothingIsAttested(t *testing.T) {
	assets := releaseAssets()
	srv := ghReleaseServer(t, "v1.2.0", assets)
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)
	swapAttestVerify(t, attestedRelease{missing: map[string]bool{
		sha256Hex(assets["demo_linux.tar.gz"]):  true,
		sha256Hex(assets["demo_darwin.tar.gz"]): true,
	}})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a release that never claimed provenance should not be red: %+v", res)
	}
	if ck := checkFor(t, res, attestCheckName); ck.Status != verifyStatusPartial {
		t.Fatalf("no provenance at all is partial, not failed: %+v", ck)
	}
	var warned bool
	for _, w := range res.Warnings {
		if w.Code == output.WarnAttestUnavailable {
			warned = true
			if !strings.Contains(w.Message, "id-token: write") {
				t.Errorf("the warning should say what makes release attach provenance: %q", w.Message)
			}
		}
	}
	if !warned {
		t.Fatalf("a release with no provenance must warn: %+v", res.Warnings)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 来歴の partial に打てる手は install ではない(workflow の permissions)。--install を勧めると、
// 入れてみれば証明が付くかのように読める。
func TestAttestPartialDoesNotAskForInstall(t *testing.T) {
	checks := []verifyCheck{{Channel: attestCheckName, Status: verifyStatusPartial}}
	if hasInstallablePartial(checks) {
		t.Error("provenance is not something --install can exercise")
	}
}

// 資産の実在照合が落ちているなら、来歴を確かめる対象そのものが揃っていない。そこで別行を立てても
// 「releases が壊れている」以上のことは言えないので、行は立てない。
func TestVerifyAttestIsNotCheckedWhenTheReleaseIsBroken(t *testing.T) {
	srv := ghReleaseServer(t, "v1.2.0", map[string]string{
		"latest.json": latestJSON("1.2.0", "demo_linux.tar.gz"), // 資産が実在しない
	})
	chdir(t, scratchReleases(t, "1.2.0"))
	swapReleasesProbe(t, srv.URL)

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK {
		t.Fatalf("a release missing its assets should fail: %+v", res)
	}
	for _, ck := range checksOf(t, res) {
		if ck.Channel == attestCheckName {
			t.Fatalf("provenance cannot be checked on a release whose artifacts are missing: %+v", ck)
		}
	}
}
