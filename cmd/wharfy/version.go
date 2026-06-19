// version.go — tag を単一ソースにする version 注入(samples/version.go を移植)。
//
// 値は手書きしない。リリース時に ldflags で注入する:
//
//	-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}
//
// これで「コードに書いたバージョン」と「実際に配ったタグ」がズレない。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// ldflags で上書きされる。未注入(go run / go install 直)のときの既定値。
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// resolveVersion は ldflags 注入が無いとき(go install など)に
// ビルド情報へフォールバックする。エージェントが版を確実に読めるようにする。
func resolveVersion() (v, c, d string) {
	v, c, d = version, commit, date
	if v != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				c = s.Value
			case "vcs.time":
				d = s.Value
			}
		}
	}
	return
}

func versionLine() string {
	v, c, d := resolveVersion()
	if c == "" {
		return v
	}
	short := c
	if len(short) > 7 {
		short = short[:7]
	}
	if d == "" {
		return fmt.Sprintf("%s (%s)", v, short)
	}
	return fmt.Sprintf("%s (%s, %s)", v, short, d)
}

// versionInfo は `wharfy version --json` の data。新版チェックの結果も載せる。
type versionInfo struct {
	Version         string `json:"version"`
	Commit          string `json:"commit,omitempty"`
	Date            string `json:"date,omitempty"`
	Latest          string `json:"latest,omitempty"` // 取得できた最新リリース tag(best-effort)
	UpdateAvailable bool   `json:"update_available"`
}

// releasesAPIURL は wharfy 自身の最新リリースを引く先(テストで差し替え可能)。
var releasesAPIURL = "https://api.github.com/repos/ShiroDoromoto/wharfy/releases/latest"

// latestReleaseTag は wharfy 自身の最新リリース tag を best-effort で返す。
// ネットワーク不通・非200・パース失敗は ("", false)(通知を出さないだけ・エラーにしない)。
// 全コマンドではなく version でのみ呼ぶ(agent 駆動の --json 契約とレイテンシを汚さない)。
func latestReleaseTag(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPIURL, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.TagName == "" {
		return "", false
	}
	return r.TagName, true
}

// updateAvailable は latest が current より新しいか。current が "dev"(未注入)や
// latest 不明なら通知しない。v 接頭辞の有無は CompareVersions が吸収する。
func updateAvailable(current, latest string) bool {
	return current != "dev" && latest != "" && state.CompareVersions(latest, current) > 0
}
