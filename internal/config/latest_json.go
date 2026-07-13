package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// latest_json.go — 更新チェックの横串になる静的 latest.json を生成する。
//
// release 時点で wharfy は全 Release アセットの URL を握っているため、これは最も低コストで
// 作れる純生成物。ビークル非依存・OS 横断で「新版あり」を知らせる土台になる。
// wharfy が持つのはここまで — アプリ内更新通知の実行時実装は各プロダクトの責務
// (任意言語のバイナリに注入できない)。wharfy は latest.json の"契約"(schemas/latest.json)を
// 定義して発行するだけで、プロダクトはこの JSON を読んで自版と比較する。

// LatestJSONName は生成物のファイル名。Release アセットとして上げると
// https://github.com/<owner>/<repo>/releases/latest/download/latest.json が常に最新版を指す。
const (
	LatestJSONName    = "latest.json"
	LatestJSONRelPath = WharfyDirName + "/" + LatestJSONName
)

// LatestAsset は latest.json が参照する 1 つの Release ダウンロード資産。
// build パッケージへの依存を避けるため config 側の最小型にする(呼び手が写し替える)。
type LatestAsset struct {
	OS   string // go の GOOS (darwin/linux/windows)
	Arch string // go の GOARCH (amd64/arm64 等)
	Name string // Release 上の資産ファイル名(basename)
}

// latestJSON は latest.json の直列化形。schemas/latest.json が契約。
type latestJSON struct {
	Version  string            `json:"version"`
	NotesURL string            `json:"notes_url,omitempty"`
	Assets   map[string]string `json:"assets"`
	// Deprecations は畳むチャネルの告知(D-3)。caveats は brew install のときにしか出ないので、
	// この経路が無いと「すでに入れた人」に届かない。プロダクトの更新チェックがここを読む。
	Deprecations map[string]latestDeprecation `json:"deprecations,omitempty"`
	// Extra は配布元が宣言する任意のメタ情報(D-236)。wharfy は中身を解釈も検証もせず逐語で運ぶ
	// (アプリのローカルデータ形式の版など、意味がアプリごとに違うものは wharfy の語彙では語れない)。
	// 名前空間を extra に閉じてあるので、wharfy が将来足すフィールドと衝突しない。
	Extra map[string]any `json:"extra,omitempty"`
}

// latestDeprecation は latest.json に載る告知 1 件。文面は配布者のものを逐語で運ぶ。
type latestDeprecation struct {
	Since   string `json:"since,omitempty"`
	Ship    bool   `json:"ship"`
	Message string `json:"message,omitempty"`
}

// latestDeprecations は解決済みチャネルから告知を集める。宣言が無ければ nil(omitempty で消える)。
func latestDeprecations(cfg Config) map[string]latestDeprecation {
	var m map[string]latestDeprecation
	for _, ch := range cfg.Channels {
		if ch.Deprecated == nil {
			continue
		}
		if m == nil {
			m = map[string]latestDeprecation{}
		}
		m[ch.Name] = latestDeprecation{
			Since:   ch.Deprecated.Since,
			Ship:    ch.Deprecated.Ship,
			Message: ch.Deprecated.Message,
		}
	}
	return m
}

// GenerateLatestJSON は version と Release 資産から latest.json 本文を返す。
// github(owner/repo)が未解決だと URL を組めないので ok=false を返し、呼び手は発行を見送る。
func GenerateLatestJSON(cfg Config, version string, assets []LatestAsset) (content string, ok bool) {
	owner, repo, ok := splitOwnerRepo(cfg.Github)
	if !ok {
		return "", false
	}
	base := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s", owner, repo, version)
	m := make(map[string]string, len(assets))
	for _, a := range assets {
		key := latestAssetKey(a.OS, a.Arch, a.Name)
		if key == "" {
			continue // 種別を割れない資産は載せない(誤った URL を書かない)
		}
		m[key] = base + "/" + a.Name
	}
	doc := latestJSON{
		Version:      version,
		NotesURL:     fmt.Sprintf("https://github.com/%s/%s/releases/tag/v%s", owner, repo, version),
		Assets:       m,
		Deprecations: latestDeprecations(cfg),
		Extra:        cfg.LatestExtra,
	}
	// map キーは encoding/json が辞書順に直列化するので出力は決定的(冪等アップロード向き)。
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", false
	}
	return string(b) + "\n", true
}

// latestAssetKey は OS/Arch/資産名から latest.json の assets キーを作る。
// アーカイブ(.tar.gz/.zip)は "<os>-<arch>"、パッケージ/バンドルは種別を付けて
// "<os>-<arch>-<kind>"(deb/rpm/dmg/appimage 等)。GoReleaser の Linux Package は Kind 空で
// archive と同じ os/arch を持つため、拡張子で厳密に種別を割る(publish.go の formulaArchives と
// 同じ理由)。os/arch が空、または種別を割れない資産は "" を返して除外する。
func latestAssetKey(goos, goarch, name string) string {
	if goos == "" || goarch == "" {
		return ""
	}
	osLabel := goos
	if goos == "darwin" {
		osLabel = "macos"
	}
	archLabel := goarch
	switch goarch {
	case "amd64":
		archLabel = "x64"
	case "386":
		archLabel = "x86"
	}
	base := osLabel + "-" + archLabel
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"), strings.HasSuffix(name, ".zip"):
		return base
	case strings.HasSuffix(name, ".deb"):
		return base + "-deb"
	case strings.HasSuffix(name, ".rpm"):
		return base + "-rpm"
	case strings.HasSuffix(name, ".dmg"):
		return base + "-dmg"
	case strings.HasSuffix(name, ".AppImage"):
		return base + "-appimage"
	case strings.HasSuffix(name, ".exe"):
		return base + "-exe"
	case strings.HasSuffix(name, ".msi"):
		return base + "-msi"
	case strings.HasSuffix(name, ".pkg"):
		return base + "-pkg"
	default:
		return ""
	}
}

// WriteLatestJSON は latest.json を <root>/.wharfy/latest.json に書く(root は汚さない)。
func WriteLatestJSON(root, content string) (string, error) {
	dir := filepath.Join(root, WharfyDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, LatestJSONName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
