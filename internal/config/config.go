// Package config は最小 wharfy.yaml を読み、既定を推測して実効設定を組み立てる
// (設計 07 / ADR-4「入力は最小宣言・出力を契約にする」)。
//
// 凍結するのは出力(schemas/config.json の resolved)であって入力ではない。
// 入力(File)はエディタ補助の助言。`wharfy config` は解決後の Config を返す(生ファイルではない)。
//
// 依存方向: ドメイン層なので上位(output/emit・CLI)を import しない。
// 失敗は code-agnostic な型で返し、Result への変換(コード付与)は CLI 層で行う。
package config

// File は wharfy.yaml の入力(schemas/wharfy.config.json に対応・助言)。全フィールド省略可。
// 未知キーは yaml.v3 が無視する(入力は厳密契約ではない)。スライス1 で解決に使うキーを持つ。
type File struct {
	Project     string         `yaml:"project"`
	Binary      string         `yaml:"binary"`
	Main        string         `yaml:"main"`
	Github      string         `yaml:"github"`
	Homepage    string         `yaml:"homepage"`
	Description string         `yaml:"description"`
	License     string         `yaml:"license"`
	Channels    []string       `yaml:"channels"`
	Build       *BuildInput    `yaml:"build"`
	Homebrew    *HomebrewInput `yaml:"homebrew"`
	Scoop       *ScoopInput    `yaml:"scoop"`
	Goinstall   *GoinstallIn   `yaml:"goinstall"`
	Apt         *RepoInput     `yaml:"apt"`
	Rpm         *RepoInput     `yaml:"rpm"`
}

// RepoInput は hosted パッケージリポジトリの設定(apt/rpm 共通)。Repo は self-host の URL。
type RepoInput struct {
	Repo string `yaml:"repo"`
}

type BuildInput struct {
	GOOS         []string `yaml:"goos"`
	GOARCH       []string `yaml:"goarch"`
	Env          []string `yaml:"env"`
	LdflagsExtra []string `yaml:"ldflags_extra"`
}

type HomebrewInput struct {
	Tap          string   `yaml:"tap"`
	Dependencies []string `yaml:"dependencies"`
}

type ScoopInput struct {
	Bucket string `yaml:"bucket"`
}

type GoinstallIn struct {
	Module string `yaml:"module"`
}

// Config は解決後の実効設定。schemas/config.json の $defs/resolved と同形。
// `wharfy config --json` の data に入る。
type Config struct {
	Project  string            `json:"project"`
	Main     string            `json:"main,omitempty"`
	Github   string            `json:"github,omitempty"`
	Homepage string            `json:"homepage,omitempty"`
	License  string            `json:"license,omitempty"`
	Channels []ResolvedChannel `json:"channels"`
	Build    *Build            `json:"build,omitempty"`
}

// ResolvedChannel は解決済みチャネル 1 つ(名前・種別・発行先)。
type ResolvedChannel struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
}

// Build は解決後のビルド対象 os/arch。
type Build struct {
	GOOS   []string `json:"goos,omitempty"`
	GOARCH []string `json:"goarch,omitempty"`
}

// 既定値(07 のフィールド一覧)。
var (
	// DefaultChannels = 追加設定不要な owned 列(goinstall は Go ターゲット時のみ)。
	DefaultChannels = []string{"homebrew", "scoop", "releases", "script", "goinstall"}
	DefaultGOOS     = []string{"linux", "darwin", "windows"}
	DefaultGOARCH   = []string{"amd64", "arm64"}
)

// channelKind は各チャネルの種別。gated は審査制(winget / *-core)。それ以外は owned。
var channelKind = map[string]string{
	"homebrew":  "owned",
	"scoop":     "owned",
	"apt":       "owned",
	"rpm":       "owned",
	"releases":  "owned",
	"script":    "owned",
	"container": "owned",
	"aur":       "owned",
	"goinstall": "owned",
	"winget":    "gated",
}

// Kind はチャネル名から種別を返す。未知は owned 扱い(既定)。
func Kind(name string) string {
	if k, ok := channelKind[name]; ok {
		return k
	}
	return "owned"
}
