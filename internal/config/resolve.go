package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileName は利用者リポジトリ root に置く設定ファイル名。
const ConfigFileName = "wharfy.yaml"

// AmbiguousMainError は main を一意に推測できないとき(黙って間違えない)。
// CLI 層がこれを output.ErrMainAmbiguous の Problem に変換する(code 付与は Result 作成側)。
type AmbiguousMainError struct {
	Candidates []string // 検出された main パッケージ(./相対)。0 件のこともある。
}

func (e *AmbiguousMainError) Error() string {
	if len(e.Candidates) == 0 {
		return "cannot detect a main package; set 'main' in wharfy.yaml"
	}
	return "multiple main packages; set 'main' in wharfy.yaml (candidates: " +
		strings.Join(e.Candidates, ", ") + ")"
}

// Resolver は実効設定を組み立てる。外部 I/O(git / go list / go.mod 読み)は
// 関数フィールドに分離してテストで差し替え可能にする(末端は差し替え可能)。
type Resolver struct {
	Root       string
	OriginURL  func(root string) (string, error)   // git remote origin URL
	MainPkgs   func(root string) ([]string, error) // ./相対の main パッケージ一覧
	ModulePath func(root string) (string, error)   // go.mod の module パス
}

// NewResolver は本番用の既定 I/O を差した Resolver を返す。
func NewResolver(root string) *Resolver {
	return &Resolver{
		Root:       root,
		OriginURL:  gitOriginURL,
		MainPkgs:   goListMainPkgs,
		ModulePath: readModulePath,
	}
}

// Load は root の wharfy.yaml を読む。無ければ空 File(エラーなし＝ほぼ空で動く前提)。
func Load(root string) (File, error) {
	path := filepath.Join(root, ConfigFileName)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("%s: %w", ConfigFileName, err)
	}
	return f, nil
}

// Resolve は解決順(フラグ＞env＞明示値＞推測)のうち、明示値＞推測を組み立てる。
// フラグ・env の上書きは CLI 層が File に流し込む前提でここでは扱わない。
func (r *Resolver) Resolve(in File) (Config, error) {
	owner, repo := splitGithub(in.Github)
	if in.Github == "" {
		if url, err := r.OriginURL(r.Root); err == nil {
			if o, rp, ok := inferGithub(url); ok {
				owner, repo = o, rp
			}
		}
	}
	github := in.Github
	if github == "" && owner != "" && repo != "" {
		github = owner + "/" + repo
	}

	project := firstNonEmpty(in.Project, repo, r.moduleLast(), filepath.Base(r.Root))

	prebuilt := IsPrebuilt(in)
	bundle := IsBundle(in)

	// main が曖昧でも、他は解決した実効設定を返す(config.json は data 必須・main は任意)。
	// 呼び出し側はこの err を ok=false + main_ambiguous の Problem に変換する(停止)。
	// BYO-binary(prebuilt)/ BYO-bundle(bundle)ではビルドを wharfy がしないので main は不要 —
	// `go list` を叩かず main は空のまま進める(非 Go リポで `cannot resolve 'main'` にならない・依頼①)。
	var main string
	var mainErr error
	if !prebuilt && !bundle {
		main, mainErr = r.resolveMain(in.Main, project)
	}

	homepage := in.Homepage
	if homepage == "" && github != "" {
		homepage = "https://github.com/" + github
	}

	license := in.License
	if license == "" {
		license = detectLicense(r.Root)
	}

	build := resolveBuild(in.Build)
	if prebuilt {
		build = prebuiltBuild(in.Prebuilt) // 行列は持ち込みバイナリの (os,arch) から導く
	}
	if bundle {
		build = bundleBuild(in.Bundle) // 行列は持ち込みバンドルの (os,arch) から導く
	}

	cfg := Config{
		Project:  project,
		Main:     main,
		Github:   github,
		Homepage: homepage,
		License:  license,
		Channels: r.resolveChannels(in, owner, github, project, prebuilt, bundle),
		Build:    build,
		Prebuilt: prebuilt,
		Bundle:   bundle,
	}
	cfg.OrphanDeprecations = orphanDeprecations(in, cfg.Channels)
	return cfg, mainErr
}

// bundleBuild は持ち込みバンドルの一覧から (os, arch) 行列を宣言順・重複なしで導く
// (prebuiltBuild の対。ビルドはしないが Config.Build の表示として持つ)。
func bundleBuild(in *BundleInput) *Build {
	var goos, goarch []string
	seenOS, seenArch := map[string]bool{}, map[string]bool{}
	for _, b := range in.Bundles {
		if b.OS != "" && !seenOS[b.OS] {
			seenOS[b.OS] = true
			goos = append(goos, b.OS)
		}
		if b.Arch != "" && !seenArch[b.Arch] {
			seenArch[b.Arch] = true
			goarch = append(goarch, b.Arch)
		}
	}
	return &Build{GOOS: goos, GOARCH: goarch}
}

// prebuiltBuild は持ち込みバイナリの一覧から、重複を除いた (os, arch) 行列を宣言順で導く。
// 既定 GOOS/GOARCH は使わない — 実際に持ち込まれたターゲットだけが配布対象(依頼①)。
func prebuiltBuild(in *PrebuiltInput) *Build {
	var goos, goarch []string
	seenOS, seenArch := map[string]bool{}, map[string]bool{}
	for _, b := range in.Binaries {
		if b.OS != "" && !seenOS[b.OS] {
			seenOS[b.OS] = true
			goos = append(goos, b.OS)
		}
		if b.Arch != "" && !seenArch[b.Arch] {
			seenArch[b.Arch] = true
			goarch = append(goarch, b.Arch)
		}
	}
	return &Build{GOOS: goos, GOARCH: goarch}
}

// resolveMain は main の明示値を優先し、無ければ検出する。曖昧なら停止。
func (r *Resolver) resolveMain(explicit, project string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cands, err := r.MainPkgs(r.Root)
	if err != nil {
		return "", err
	}
	preferred := "./cmd/" + project
	for _, c := range cands {
		if c == preferred {
			return preferred, nil // ./cmd/<project> があれば優先
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	return "", &AmbiguousMainError{Candidates: cands}
}

// resolveChannels は channels の明示値 or 既定列を、種別・発行先まで解決する。
// prebuilt(BYO-binary)では Go 専用チャネル(goinstall/homebrew-core)を落とす(依頼②)。
// 明示指定を静かに握り潰さないため、落とした分は CLI 層が要求列と突き合わせて警告できる。
func (r *Resolver) resolveChannels(in File, owner, github, project string, prebuilt, bundle bool) []ResolvedChannel {
	names := in.Channels
	if len(names) == 0 {
		switch {
		case bundle:
			names = DefaultBundleChannels // GUI: cask ＋ releases(依頼書 §6)
		case prebuilt:
			names = DefaultPrebuiltChannels
		default:
			names = DefaultChannels
		}
	}
	out := make([]ResolvedChannel, 0, len(names))
	for _, name := range names {
		if prebuilt && !PrebuiltCompatible(name) {
			continue // Go 専用チャネルは非 Go では成立しないので外す(依頼②)
		}
		ch := ResolvedChannel{Name: name, Kind: Kind(name)}
		switch name {
		case "apt":
			ch.Target, ch.PushTarget = resolveRepoURLs(name, in.Apt)
		case "rpm":
			ch.Target, ch.PushTarget = resolveRepoURLs(name, in.Rpm)
		default:
			ch.Target = r.channelTarget(name, in, owner, github, project)
		}
		ch.Deprecated = resolveDeprecation(name, in.Deprecate[name])
		out = append(out, ch)
	}
	return out
}

// resolveDeprecation は deprecate 宣言 1 つを解決する(D-3)。未宣言なら nil を返し、
// 生成物は 1 バイトも変わらない。文面は逐語で運ぶ(wharfy は言い換えない)。
func resolveDeprecation(channel string, in *DeprecateInput) *Deprecation {
	if in == nil {
		return nil
	}
	ship := true // 既定。畳むと決めた瞬間に入手経路が切れると事故になる
	if in.Ship != nil {
		ship = *in.Ship
	}
	return &Deprecation{
		Since:         in.Since,
		Ship:          ship,
		Message:       in.Message,
		NoticeSurface: HasNoticeSurface(channel),
	}
}

// orphanDeprecations は channels に載っていないチャネルへの deprecate 宣言を名前順で返す。
// 畳んだうえで channels からも外すと、wharfy はそのチャネルを触らなくなり告知の更新が止まる。
// 禁じはしない(既存の成果物は残る)が、黙ってもいない。
func orphanDeprecations(in File, resolved []ResolvedChannel) []string {
	if len(in.Deprecate) == 0 {
		return nil
	}
	live := make(map[string]bool, len(resolved))
	for _, ch := range resolved {
		live[ch.Name] = true
	}
	var out []string
	for name := range in.Deprecate {
		if !live[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out) // map 反復順は不定。出力を決定的にする
	return out
}

// fury.io(Gemfury)の URL 規則。push と配信が別ホストで、配信は apt/rpm でホストが異なる。
const (
	furyPushHost = "https://push.fury.io"
	furyAptHost  = "https://apt.fury.io"
	furyYumHost  = "https://yum.fury.io"
)

// resolveRepoURLs は apt/rpm の配信(probe/install)とアップロード(push)URL を解決する。
//   - Provider+User 指定: プロバイダ規則から自動導出(fury のみ実装。手間最小の推奨経路)。
//   - 生 URL 指定: deliver=Repo、push=Push(空なら Repo にフォールバック=push と配信が同一の汎用ホスト)。
//
// in が nil(未設定)なら両方空 → publish で skip 案内になる。
func resolveRepoURLs(channel string, in *RepoInput) (deliver, push string) {
	if in == nil {
		return "", ""
	}
	if in.Provider == "fury" && in.User != "" {
		host := furyAptHost
		if channel == "rpm" {
			host = furyYumHost
		}
		return host + "/" + in.User + "/", furyPushHost + "/" + in.User + "/"
	}
	return in.Repo, firstNonEmpty(in.Push, in.Repo)
}

// channelTarget は自前 tap/bucket 等の発行先を既定生成する(プロジェクトごと命名)。
// 解決できない(owner 不明など)場合は空(schema 上 target は任意)。
func (r *Resolver) channelTarget(name string, in File, owner, github, project string) string {
	switch name {
	case "homebrew":
		if in.Homebrew != nil && in.Homebrew.Tap != "" {
			return in.Homebrew.Tap
		}
		if owner != "" {
			return fmt.Sprintf("%s/homebrew-%s", owner, project)
		}
	case "cask":
		// cask を置く tap。既定は Formula と同じ <owner>/homebrew-<project>(同居=状態一元化・依頼④)。
		if in.Cask != nil && in.Cask.Tap != "" {
			return in.Cask.Tap
		}
		if in.Homebrew != nil && in.Homebrew.Tap != "" {
			return in.Homebrew.Tap
		}
		if owner != "" {
			return fmt.Sprintf("%s/homebrew-%s", owner, project)
		}
	case "scoop":
		if in.Scoop != nil && in.Scoop.Bucket != "" {
			return in.Scoop.Bucket
		}
		if owner != "" {
			return fmt.Sprintf("%s/scoop-%s", owner, project)
		}
	case "releases":
		// 発行先は実リポジトリ(github の owner/repo)。project 名と repo 名が違う場合に
		// owner/project だと実体とズレるため github をそのまま使う。
		return github
	case "goinstall":
		if in.Goinstall != nil && in.Goinstall.Module != "" {
			return in.Goinstall.Module
		}
		if mod, err := r.ModulePath(r.Root); err == nil && mod != "" {
			return mod
		}
	case "script":
		// base_url 指定時は vanity URL の install.sh を発行先にする。
		// 未指定なら空のまま → InstallURL が GitHub Releases の latest を既定にする(後方互換)。
		if in.Script != nil && in.Script.BaseURL != "" {
			return strings.TrimRight(in.Script.BaseURL, "/") + "/" + InstallScriptName
		}
	// apt/rpm は配信/push を分けるため resolveChannels が resolveRepoURLs で解決する(ここには来ない)。
	case "container":
		// OCI イメージ名。既定 ghcr.io/<owner>/<project>(ghcr は小文字必須)。
		if in.Container != nil && in.Container.Image != "" {
			return in.Container.Image
		}
		if owner != "" {
			return "ghcr.io/" + strings.ToLower(owner) + "/" + strings.ToLower(project)
		}
	case "winget":
		// PackageIdentifier。既定 <Owner>.<Project>(winget は大小区別あり・そのまま)。
		if in.Winget != nil && in.Winget.Identifier != "" {
			return in.Winget.Identifier
		}
		if owner != "" {
			return owner + "." + project
		}
	case "aur":
		// AUR パッケージ名。既定 <project>-bin(ビルド済みバイナリ慣習)。
		if in.Aur != nil && in.Aur.Package != "" {
			return in.Aur.Package
		}
		return project + "-bin"
	case "homebrew-core":
		// 上流の中央 repo(固定)。formula 名は project。
		return "Homebrew/homebrew-core"
	}
	return ""
}

func resolveBuild(in *BuildInput) *Build {
	goos := DefaultGOOS
	goarch := DefaultGOARCH
	if in != nil {
		if len(in.GOOS) > 0 {
			goos = in.GOOS
		}
		if len(in.GOARCH) > 0 {
			goarch = in.GOARCH
		}
	}
	return &Build{GOOS: goos, GOARCH: goarch}
}

func (r *Resolver) moduleLast() string {
	mod, err := r.ModulePath(r.Root)
	if err != nil || mod == "" {
		return ""
	}
	return baseName(mod)
}

// --- 既定の外部 I/O 実装(末端) ---

// execError は外部コマンドの失敗を、実行内容と捕捉した stderr つきで包む。
// exec.ExitError の .Error() は "exit status 1" しか返さず、真因は捨てられる Stderr に埋もれる。
// 呼び出し側(config 等)が文脈を失ったまま internal error として見せてしまうのを防ぐ。
func execError(what string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if stderr := strings.TrimSpace(string(ee.Stderr)); stderr != "" {
			return fmt.Errorf("%s: %w: %s", what, err, stderr)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func gitOriginURL(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", execError("git remote get-url origin", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// goListMainPkgs は go list で main パッケージを列挙し ./相対 に直す。
// go list は build 制約を尊重するので //go:build ignore のスケッチは除外される。
func goListMainPkgs(root string) ([]string, error) {
	cmd := exec.Command("go", "list", "-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`, "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, execError("go list ./...", err)
	}
	mod, _ := readModulePath(root)
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pkgs = append(pkgs, toRelImport(line, mod))
	}
	return pkgs, nil
}

func readModulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module")), nil
		}
	}
	return "", fmt.Errorf("no module directive in go.mod")
}

// --- 純関数ヘルパ ---

var githubRe = regexp.MustCompile(`github\.com[/:]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)

// inferGithub は git remote URL(https / ssh)から owner/repo を抜く。github.com 以外は不可。
func inferGithub(url string) (owner, repo string, ok bool) {
	m := githubRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func splitGithub(s string) (owner, repo string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", ""
}

// toRelImport は import パスを module 基準の ./相対 に直す。module 直下なら "."。
func toRelImport(importPath, mod string) string {
	if mod == "" {
		return importPath
	}
	if importPath == mod {
		return "."
	}
	if rest := strings.TrimPrefix(importPath, mod+"/"); rest != importPath {
		return "./" + rest
	}
	return importPath
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// detectLicense は LICENSE ファイルから SPDX を粗く推定する。不確実なら空。
func detectLicense(root string) string {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		return spdxFromText(string(b))
	}
	return ""
}

func spdxFromText(text string) string {
	head := text
	if len(head) > 4096 {
		head = head[:4096]
	}
	switch {
	case strings.Contains(head, "GNU AFFERO GENERAL PUBLIC LICENSE") && strings.Contains(head, "Version 3"):
		return "AGPL-3.0"
	case strings.Contains(head, "GNU GENERAL PUBLIC LICENSE") && strings.Contains(head, "Version 3"):
		return "GPL-3.0"
	case strings.Contains(head, "GNU LESSER GENERAL PUBLIC LICENSE") && strings.Contains(head, "Version 3"):
		return "LGPL-3.0"
	case strings.Contains(head, "Apache License") && strings.Contains(head, "Version 2.0"):
		return "Apache-2.0"
	case strings.Contains(head, "MIT License"):
		return "MIT"
	case strings.Contains(head, "Mozilla Public License") && strings.Contains(head, "2.0"):
		return "MPL-2.0"
	case strings.Contains(head, "Redistribution and use") && strings.Contains(head, "3. "):
		return "BSD-3-Clause"
	}
	return ""
}
