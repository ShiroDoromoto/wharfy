// Package config は最小 wharfy.yaml を読み、既定を推測して実効設定を組み立てる
// (入力は最小宣言・出力を契約にする)。
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
	Project     string          `yaml:"project"`
	Binary      string          `yaml:"binary"`
	Main        string          `yaml:"main"`
	Github      string          `yaml:"github"`
	Homepage    string          `yaml:"homepage"`
	Description string          `yaml:"description"`
	License     string          `yaml:"license"`
	Channels    []string        `yaml:"channels"`
	RuntimeDeps []RuntimeDep    `yaml:"runtime_deps"`
	Prebuilt    *PrebuiltInput  `yaml:"prebuilt"`
	Bundle      *BundleInput    `yaml:"bundle"`
	Build       *BuildInput     `yaml:"build"`
	Sign        *SignInput      `yaml:"sign"`
	Homebrew    *HomebrewInput  `yaml:"homebrew"`
	Cask        *CaskInput      `yaml:"cask"`
	Scoop       *ScoopInput     `yaml:"scoop"`
	Goinstall   *GoinstallIn    `yaml:"goinstall"`
	Apt         *RepoInput      `yaml:"apt"`
	Rpm         *RepoInput      `yaml:"rpm"`
	Container   *ContainerInput `yaml:"container"`
	Winget      *WingetIn       `yaml:"winget"`
	Aur         *AurIn          `yaml:"aur"`
	Script      *ScriptInput    `yaml:"script"`
	Verify      *VerifyInput    `yaml:"verify"`
	// Deprecate はチャネルを畳む宣言(D-3)。キーはチャネル名。畳んでも channels からは外さない —
	// 外すと wharfy がそのチャネルを知らなくなり、告知を運ぶ主体がいなくなる。
	Deprecate map[string]*DeprecateInput `yaml:"deprecate"`
}

// VerifyInput は verify のコンテナ検証を配布者の実態に寄せる設定。どちらも未指定で従来どおり。
//   - Images: チャネル名 → ベースイメージ。既定は debian/fedora だが、実際に配る先が
//     ubuntu / rocky なら、そこで通ることを確かめないと検証が的外れになる。
//   - Run: 入ったバイナリに渡す起動確認の引数。既定は --version → version → --help の連鎖で、
//     どれも受け付けない CLI(サブコマンド必須など)は誤って落ちる。そこを名指しで置き換える。
type VerifyInput struct {
	Images map[string]string `yaml:"images"`
	Run    []string          `yaml:"run"`
}

// DeprecateInput は畳むチャネル 1 つの宣言(D-3)。
// 文面(Message)は配布者が書く。wharfy は作らないし、言い換えもしない。
//   - Ship: 新版を配り続けるか。nil=既定 true(移行期間)。false で最後に配った版に凍結。
//     既定を true にしているのは、畳むと決めた瞬間に入手経路が切れると事故になるから。
type DeprecateInput struct {
	Since   string `yaml:"since"`
	Ship    *bool  `yaml:"ship"`
	Message string `yaml:"message"`
}

// RuntimeDep は横断ランタイム依存(B: 実行時に呼ぶ外部ツール)の宣言。1 ツール 1 エントリで、
// 全 owned パッケージチャネル(homebrew/scoop/apt/rpm/aur)へ射影する。
//   - Min: 最小バージョン。apt/rpm/aur は制約として反映、homebrew/scoop は名前のみに縮退。
//   - Required: 既定 true。false なら推奨/任意側へ(apt/rpm の Recommends・aur の optdepends)。
//     必須/任意を区別しない homebrew/scoop では required=false は出さない(As で明示時を除く)。
//   - As: チャネル別オーバーライド。値は wharfy が解釈せず逐語(verbatim)で出力する逃げ道。
//     名前がディストロで違う場合や、チャネル固有記法を書きたい場合に使う。
type RuntimeDep struct {
	Name     string            `yaml:"name"`
	Min      string            `yaml:"min"`
	Required *bool             `yaml:"required"` // nil=既定 true
	As       map[string]string `yaml:"as"`
}

// ScriptInput は curl|sh インストーラ install.sh の設定。
// BaseURL は install.sh の公開 URL ベース(vanity ドメイン / CDN 等)。
// 既定(空)は GitHub Releases の latest アセット。利用者案内・status・probe がここを見る。
type ScriptInput struct {
	BaseURL string `yaml:"base_url"`
}

// AurIn は AUR の設定。Package は既定 <project>-bin を上書き。
type AurIn struct {
	Package string `yaml:"package"`
}

// WingetIn は winget の設定。Identifier は既定 <Owner>.<Project> を上書き(PackageIdentifier)。
type WingetIn struct {
	Identifier string `yaml:"identifier"`
}

// RepoInput は hosted パッケージリポジトリの設定(apt/rpm 共通)。
// 配信(probe/install)と push(アップロード)が別ホストなホスト型サービス(fury.io 等)に対応する。
//   - Provider+User を指定すると push/配信 URL をプロバイダ規則から自動導出する(手間最小)。
//   - 生 URL で書く場合は Repo(配信) と Push(アップロード) を指定する。
//     Push 省略時は Repo にフォールバック(push=配信が同一の汎用ホスト用・後方互換)。
type RepoInput struct {
	Repo     string `yaml:"repo"`     // 配信 URL(probe/install)。provider 指定時は自動導出
	Push     string `yaml:"push"`     // アップロード URL。空なら Repo にフォールバック
	Provider string `yaml:"provider"` // "fury" 等。User から push/配信を導出する
	User     string `yaml:"user"`     // provider の名前空間(fury のユーザー名)
	// ランタイム依存。deb/rpm の native 3 区分に対応(必須/推奨/提案)。
	// 単一 nfpm エントリの overrides.<format> に振り分けるため apt と rpm で別々に書ける
	// (依存パッケージ名はディストロで異なるため)。空なら出力に出ない(後方互換)。
	Depends    []string `yaml:"depends"`    // 必須(deb Depends / rpm Requires)
	Recommends []string `yaml:"recommends"` // 推奨(deb Recommends / rpm 弱依存)
	Suggests   []string `yaml:"suggests"`   // 提案(deb Suggests)
}

// ContainerInput は OCI イメージの設定。Image は既定 ghcr.io/<owner>/<project> を上書き。
type ContainerInput struct {
	Image string `yaml:"image"`
}

type BuildInput struct {
	GOOS         []string `yaml:"goos"`
	GOARCH       []string `yaml:"goarch"`
	Env          []string `yaml:"env"`
	LdflagsExtra []string `yaml:"ldflags_extra"`
}

// PrebuiltInput は BYO-binary モードの入力(言語非依存化・依頼①)。
// 宣言があると **ビルド(コンパイル)責務は利用者側へ移る**: wharfy は自前ビルドせず、
// ここで渡されたビルド済みバイナリを package→release→publish に流すだけになる。
// これにより Go 以外(Rust/cargo 等)のプロジェクトも載る。GoReleaser のビルド行列
// (goos/goarch/main/ldflags)は使わない — 版はバイナリ側で確定済みが前提。
//   - Version: 任意。空なら git tag が版の真実(既存挙動と同じ)。
//   - Binaries: (os,arch)→ビルド済みバイナリのパス。最低 1 要素。
type PrebuiltInput struct {
	Version  string           `yaml:"version"`
	Binaries []PrebuiltBinary `yaml:"binaries"`
}

// PrebuiltBinary は 1 ターゲットのビルド済みバイナリ。
//   - OS/Arch: 配布ターゲット(common.json の os/arch と同じ語彙: linux/darwin/windows, amd64/arm64)。
//   - Path: 利用者 root からの相対パス。wharfy はここを archive 化・checksum する。
//   - SHA256: 任意。空なら wharfy が計算する(持ち込み時の検証にも使える)。
type PrebuiltBinary struct {
	OS     string `yaml:"os"`
	Arch   string `yaml:"arch"`
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// BundleInput は BYO-bundle モードの入力(GUI・依頼①)。prebuilt(単一バイナリ持ち込み)の対で、
// ビルド済み・署名済みのバンドル(.dmg/.zip 等)を持ち込む。wharfy は生成も再署名もしない(relay)。
// 宣言があると Config.Bundle=true になり、既定チャネルは GUI 向け(cask/releases)に切り替わる。
// バンドルは既に最終成果物なので prebuilt と違い再 archive しない — 存在と sha256 を検証して
// そのまま Release アセットにする。
//   - Version: 任意。空なら git tag が版の真実(prebuilt と同じ)。
//   - Name: アプリ表示名 "<App>"(cask の name / app stanza)。空なら project。token とは独立・不変。
//   - Bundles: (os,arch,種別)→バンドルのパス。最低 1 要素。
type BundleInput struct {
	Version string   `yaml:"version"`
	Name    string   `yaml:"name"`
	Bundles []Bundle `yaml:"bundles"`
}

// Bundle は 1 ターゲットのビルド済みバンドル。
//   - OS/Arch: 配布ターゲット(darwin/windows/linux, amd64/arm64/universal)。
//   - Kind: バンドル種別(dmg/zip/exe/msi/appimage/deb/rpm)。Cask は darwin の dmg/zip を参照する。
//   - Path: 利用者 root からの相対パス。wharfy はここを Release アセットにする(再 archive しない)。
//   - SHA256: 任意。空なら wharfy が計算する(持ち込み時の検証にも使える)。
type Bundle struct {
	OS     string `yaml:"os"`
	Arch   string `yaml:"arch"`
	Kind   string `yaml:"kind"`
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// SignInput は sign 段の設定(依頼①: sign を advisory から実署名へ)。
// identity が解決できなければ sign は従来どおり no-op(pre-signed 持ち込みを尊重)。
// **秘密(P12 パスワード)は yaml にも生成物にも書かない** — env からのみ取る。
//   - Identity: codesign の --sign 引数(キーチェーン上の証明書名。例 "Developer ID Application: … (TEAMID)")。
//     空なら env WHARFY_SIGN_IDENTITY にフォールバック。これがローカル署名(証明書がキーチェーン常駐)の経路。
//   - P12: 可搬 PKCS#12(.p12)のパス。指定すると wharfy が一時キーチェーンに import してから署名し、後始末する
//     (CI 等・証明書がキーチェーンに無い環境向け)。空なら env WHARFY_SIGN_P12。
//     パスワードは env WHARFY_SIGN_P12_PASSWORD からのみ。P12 使用時も Identity(証明書名)は必須。
type SignInput struct {
	Identity string `yaml:"identity"`
	P12      string `yaml:"p12"`
}

type HomebrewInput struct {
	Tap          string   `yaml:"tap"`
	Dependencies []string `yaml:"dependencies"`
}

// CaskInput は Homebrew Cask チャネルの設定。token / 表示名を Formula と独立に持てる(依頼②/⑥)。
// 命名規約(依頼書 §4): 配布 token は GUI=<project>-app(Formula の <project> と別ラベル)だが、
// コマンド名・アプリ表示名は不変。分けるのは配布ラベルだけ。
//   - Token: cask の識別子/ファイル名。既定 <project>-app。
//   - Tap: cask を置く tap。既定 <owner>/homebrew-<project>(Formula と同居させ状態を一元化・依頼④)。
//   - Name: cask の name stanza(表示名 "<App>")。空なら Bundle.Name → project。
//   - App: app stanza の対象 "<App>.app"。空なら "<Name>.app"。
type CaskInput struct {
	Token string `yaml:"token"`
	Tap   string `yaml:"tap"`
	Name  string `yaml:"name"`
	App   string `yaml:"app"`
}

type ScoopInput struct {
	Bucket       string   `yaml:"bucket"`
	Dependencies []string `yaml:"dependencies"`
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
	// Prebuilt=true は BYO-binary モード(依頼①)。ビルドは利用者側で済み、
	// wharfy は配布(package 以降)だけを担う。true のとき Main は空で、Build 行列は
	// 持ち込みバイナリの (os,arch) 由来。Go 専用チャネル(goinstall/homebrew-core)は外れる。
	Prebuilt bool `json:"prebuilt,omitempty"`
	// Bundle=true は BYO-bundle モード(GUI・依頼①)。バンドル生成・署名は利用者側で済み、
	// wharfy は Release アップロードと cask 等 GUI チャネルへの配布だけを担う。true のとき
	// 既定チャネルは GUI 向け(cask/releases)。Prebuilt と同じく Main は不要。
	Bundle bool `json:"bundle,omitempty"`
	// OrphanDeprecations は channels に無いチャネルへの deprecate 宣言(名前の昇順)。
	// 畳んだうえで channels からも外すと告知の更新が止まる。禁じはしないが、黙ってもいない。
	OrphanDeprecations []string `json:"orphan_deprecations,omitempty"`
}

// ResolvedChannel は解決済みチャネル 1 つ(名前・種別・発行先)。
// Target は配信先(probe/install)。PushTarget はアップロード先で、配信と push が別ホストな
// apt/rpm(fury.io 等)でのみ使う。Target と同一なら省略(omitempty)。
type ResolvedChannel struct {
	Name       string       `json:"name"`
	Kind       string       `json:"kind"`
	Target     string       `json:"target,omitempty"`
	PushTarget string       `json:"push_target,omitempty"`
	Deprecated *Deprecation `json:"deprecated,omitempty"`
}

// Deprecation は解決済みの「畳む」宣言(D-3)。
type Deprecation struct {
	Since   string `json:"since,omitempty"`
	Ship    bool   `json:"ship"`
	Message string `json:"message,omitempty"`
	// NoticeSurface はこのチャネルが告知を載せる欄を持つか(caveats / notes / description 等)。
	// false のチャネル(goinstall / container 等)では告知は latest.json 経由でしか届かない。
	// 黙って落とすと配布者は「告知したつもり」で気づけないので、status / publish が明示する。
	NoticeSurface bool `json:"notice_surface"`
}

// channelNoticeSurface は配布者の文面を載せる欄を持つチャネル(D-3)。
// 実際に流し込むのは別タスク(#20)。ここでは「載せられるか」だけを知る。
var channelNoticeSurface = map[string]bool{
	"homebrew": true, // caveats
	"cask":     true, // caveats
	"scoop":    true, // notes
	"apt":      true, // package description
	"rpm":      true, // package description
	"aur":      true, // pkgdesc
	"script":   true, // install.sh / install.ps1 の実行時 note
}

// HasNoticeSurface はチャネルが告知を載せる欄を持つか。
func HasNoticeSurface(channel string) bool { return channelNoticeSurface[channel] }

// Build は解決後のビルド対象 os/arch。
type Build struct {
	GOOS   []string `json:"goos,omitempty"`
	GOARCH []string `json:"goarch,omitempty"`
}

// 既定値。
var (
	// DefaultChannels = 追加設定不要な owned 列(goinstall は Go ターゲット時のみ)。
	DefaultChannels = []string{"homebrew", "scoop", "releases", "script", "goinstall"}
	// DefaultPrebuiltChannels = BYO-binary(非 Go)時の既定列。goinstall は Go 専用なので外す(依頼②)。
	DefaultPrebuiltChannels = []string{"homebrew", "scoop", "releases", "script"}
	// DefaultBundleChannels = BYO-bundle(GUI)時の既定列。最小構成は Cask ＋ GitHub Release
	// (依頼書 §6)。winget/scoop app・AppImage・apt/rpm GUI は明示指定で足す(依頼③)。
	DefaultBundleChannels = []string{"cask", "releases"}
	DefaultGOOS           = []string{"linux", "darwin", "windows"}
	DefaultGOARCH         = []string{"amd64", "arm64"}
)

// goOnlyChannels は Go ツールチェーンを前提とし、BYO-binary(非 Go)では成立しないチャネル(依頼②)。
//   - goinstall: そもそも `go install` 専用。
//   - homebrew-core: wharfy が生成する source-build formula が `go build` 前提。
var goOnlyChannels = map[string]bool{
	"goinstall":     true,
	"homebrew-core": true,
}

// PrebuiltCompatible は channel が BYO-binary モードで成立するかを返す(false=Go 専用で外す)。
func PrebuiltCompatible(name string) bool { return !goOnlyChannels[name] }

// IsPrebuilt は File が BYO-binary モード(ビルド済みバイナリ持ち込み)かを返す。
func IsPrebuilt(in File) bool {
	return in.Prebuilt != nil && len(in.Prebuilt.Binaries) > 0
}

// IsBundle は File が BYO-bundle モード(ビルド済みバンドル持ち込み・GUI)かを返す。
func IsBundle(in File) bool {
	return in.Bundle != nil && len(in.Bundle.Bundles) > 0
}

// channelKind は各チャネルの種別。gated は審査制(winget / *-core)。それ以外は owned。
var channelKind = map[string]string{
	"homebrew":      "owned",
	"cask":          "owned",
	"scoop":         "owned",
	"apt":           "owned",
	"rpm":           "owned",
	"releases":      "owned",
	"script":        "owned",
	"container":     "owned",
	"aur":           "owned",
	"goinstall":     "owned",
	"winget":        "gated",
	"homebrew-core": "gated", // 中央キュレーション repo(Homebrew/homebrew-core)への gated PR
}

// KnownChannel は wharfy が名前を知っているチャネルかを返す(綴り違いと「設定に無い」を分けるため)。
func KnownChannel(name string) bool {
	_, ok := channelKind[name]
	return ok
}

// Kind はチャネル名から種別を返す。未知は owned 扱い(既定)。
func Kind(name string) string {
	if k, ok := channelKind[name]; ok {
		return k
	}
	return "owned"
}
