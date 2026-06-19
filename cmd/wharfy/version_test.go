package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain は package 全体のテストを外部依存から隔離する(設計: go test に network/keychain 不要)。
// 新版チェックの既定 URL を無効化し、keyring を in-memory mock にする
// (resolveToken 経由で実 OS keychain を触らないように)。
func TestMain(m *testing.M) {
	releasesAPIURL = ""
	keyring.MockInit()
	os.Exit(m.Run())
}

func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.2.2", "v0.2.3", true},  // 新版あり(tag の v 接頭辞は無視)
		{"0.2.2", "0.2.2", false},  // 同一
		{"0.2.3", "v0.2.2", false}, // ローカルの方が新しい
		{"dev", "v9.9.9", false},   // 未注入(go run)は通知しない
		{"0.2.2", "", false},       // latest 取得不可
	}
	for _, c := range cases {
		if got := updateAvailable(c.current, c.latest); got != c.want {
			t.Errorf("updateAvailable(%q,%q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// latestReleaseTag: GitHub API をモックして tag_name を読む。
func TestLatestReleaseTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.3","name":"v0.2.3"}`))
	}))
	defer srv.Close()
	defer func(old string) { releasesAPIURL = old }(releasesAPIURL)
	releasesAPIURL = srv.URL

	tag, ok := latestReleaseTag(context.Background())
	if !ok || tag != "v0.2.3" {
		t.Errorf("latestReleaseTag = (%q,%v), want (v0.2.3,true)", tag, ok)
	}
}

// 非200(例: レート制限)は通知を出さない(エラーにしない)。
func TestLatestReleaseTagNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	defer func(old string) { releasesAPIURL = old }(releasesAPIURL)
	releasesAPIURL = srv.URL

	if tag, ok := latestReleaseTag(context.Background()); ok || tag != "" {
		t.Errorf("non-200 should yield (\"\",false), got (%q,%v)", tag, ok)
	}
}

// runVersion: 最新取得不可でも落ちず、envelope は valid・data は versionInfo。
func TestRunVersionOfflineSilent(t *testing.T) {
	defer func(old string) { releasesAPIURL = old }(releasesAPIURL)
	releasesAPIURL = "http://127.0.0.1:0/unreachable" // 接続失敗 → best-effort で無視

	res := runVersion(context.Background(), mustLookup(t, "version"), nil)
	if !res.OK {
		t.Fatalf("version should stay ok: %+v", res)
	}
	info, ok := res.Data.(versionInfo)
	if !ok {
		t.Fatalf("data should be versionInfo, got %T", res.Data)
	}
	if info.UpdateAvailable {
		t.Errorf("offline should not report update available: %+v", info)
	}
}
