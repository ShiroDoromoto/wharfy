package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// ghPrereleaseServer は tag の Release を **prerelease として** 返す最小の GitHub API。
// releases/latest は 404 —— GitHub は prerelease を latest として配らない(そこが窓の要)。
func ghPrereleaseServer(t *testing.T, tag string, assets map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/demo/releases/tags/"+tag:
			list := make([]map[string]string, 0, len(assets))
			for name, body := range assets {
				list = append(list, map[string]string{
					"name":                 name,
					"browser_download_url": srv.URL + "/dl/" + name,
					"digest":               "sha256:" + sha256Hex(body),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"prerelease": true, "assets": list})
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

// TestVerifyPrereleaseStaysGreen: prerelease の資産は普通に検証でき、しかし「まだ利用者には
// 届いていない」と言う。赤にはしない —— 検証はまさにこの窓で回すもので、赤くすれば窓が死ぬ。
// 昇格して初めて書かれるチャネル(tap)は、旧版のままで当たり前なので赤くせず skip する。
func TestVerifyPrereleaseStaysGreen(t *testing.T) {
	srv := ghPrereleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases, homebrew]\ngithub: acme/demo\n")
	chdir(t, root)
	swapReleasesProbe(t, srv.URL)

	// isPrerelease が見る先(Release の公開状態)。
	store := channel.NewInMemoryReleaseStore()
	if err := store.Upload(context.Background(), "v1.2.0", "demo 1.2.0",
		[]channel.ReleaseAsset{{Name: "demo_linux.tar.gz"}}, channel.ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	defer swapReleaseStore(store)()

	flagVerifyVersion = "1.2.0"
	defer func() { flagVerifyVersion = "" }()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("a prerelease is exactly what verify is for — it must not be red: %+v", res)
	}
	if ck := checkFor(t, res, "releases"); ck.Status == verifyStatusFailed {
		t.Errorf("the release assets are all there: %+v", ck)
	}
	if !hasWarning(res, output.WarnPrereleaseNotLatest) {
		t.Errorf("verify must say the bytes it checked are not what users get yet: %v", warnCodes(res.Warnings))
	}
	hb := checkFor(t, res, "homebrew")
	if hb.Status != verifyStatusSkipped || !strings.Contains(hb.Message, "prerelease") {
		t.Errorf("the tap cannot have an unpromoted version — that is not a failure: %+v", hb)
	}
}

// TestVerifyPrereleaseChecksInstaller: install.sh は一番踏まれる導線なので、昇格前でも確かめる。
// 実物は tag 直リンクの資産として既に上がっている —— skip すれば、配る前に一度も踏まないまま配ることになる。
func TestVerifyPrereleaseChecksInstaller(t *testing.T) {
	gh := ghPrereleaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSON("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [releases, script]\ngithub: acme/demo\n")
	chdir(t, root)
	swapReleasesProbe(t, gh.URL)
	defer swapScriptProbeURL(installScriptServer(t, "1.2.0").URL)()

	store := channel.NewInMemoryReleaseStore()
	if err := store.Upload(context.Background(), "v1.2.0", "demo 1.2.0",
		[]channel.ReleaseAsset{{Name: "demo_linux.tar.gz"}}, channel.ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	defer swapReleaseStore(store)()

	flagVerifyVersion = "1.2.0"
	defer func() { flagVerifyVersion = "" }()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("the installer on the prerelease is exactly what verify is for: %+v", res)
	}
	sc := checkFor(t, res, "script")
	if sc.Status == verifyStatusSkipped {
		t.Fatalf("install.sh is on the tagged release: it must be checked before it ships, not skipped: %+v", sc)
	}
	if sc.Status == verifyStatusFailed {
		t.Fatalf("the installer serves the expected version: %+v", sc)
	}
	// 同じ事実を 2 度言わない(releases と script の両方が prerelease を見ている)。
	if n := countWarnings(res, output.WarnPrereleaseNotLatest); n != 1 {
		t.Errorf("the release is one prerelease, so it is said once: got %d", n)
	}
}

// TestScriptVerifyURLPrereleaseIsTagDirect: 昇格前に見る URL は tag 直リンク(latest は旧版を返す)。
// 昇格した後は利用者が踏む URL そのもの —— 「いま踏む物」と「これから配る実物」を言い分ける。
func TestScriptVerifyURLPrereleaseIsTagDirect(t *testing.T) {
	cfg := config.Config{Github: "acme/demo"}
	pre := scriptVerifyURL(cfg, "1.2.0", true)
	if want := "https://github.com/acme/demo/releases/download/v1.2.0/install.sh"; pre != want {
		t.Errorf("prerelease install.sh must be read from the tag: got %q, want %q", pre, want)
	}
	promoted := scriptVerifyURL(cfg, "1.2.0", false)
	if want := "https://github.com/acme/demo/releases/latest/download/install.sh"; promoted != want {
		t.Errorf("a promoted release is read where users read it: got %q, want %q", promoted, want)
	}
}
