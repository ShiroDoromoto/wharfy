package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileName は利用者リポジトリ root に置く設定ファイル名。
const ConfigFileName = "wharfy.yaml"

// AmbiguousMainError は main を一意に推測できないとき(設計 07「黙って間違えない」)。
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
// 関数フィールドに分離してテストで差し替え可能にする(設計 01 末端は差し替え可能)。
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

// Resolve は解決順(07: フラグ＞env＞明示値＞推測)のうち、明示値＞推測を組み立てる。
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

	// main が曖昧でも、他は解決した実効設定を返す(config.json は data 必須・main は任意)。
	// 呼び出し側はこの err を ok=false + main_ambiguous の Problem に変換する(07「停止」)。
	main, mainErr := r.resolveMain(in.Main, project)

	homepage := in.Homepage
	if homepage == "" && github != "" {
		homepage = "https://github.com/" + github
	}

	license := in.License
	if license == "" {
		license = detectLicense(r.Root)
	}

	cfg := Config{
		Project:  project,
		Main:     main,
		Github:   github,
		Homepage: homepage,
		License:  license,
		Channels: r.resolveChannels(in, owner, project),
		Build:    resolveBuild(in.Build),
	}
	return cfg, mainErr
}

// resolveMain は main の明示値を優先し、無ければ検出する。曖昧なら停止(07)。
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
func (r *Resolver) resolveChannels(in File, owner, project string) []ResolvedChannel {
	names := in.Channels
	if len(names) == 0 {
		names = DefaultChannels
	}
	out := make([]ResolvedChannel, 0, len(names))
	for _, name := range names {
		out = append(out, ResolvedChannel{
			Name:   name,
			Kind:   Kind(name),
			Target: r.channelTarget(name, in, owner, project),
		})
	}
	return out
}

// channelTarget は自前 tap/bucket 等の発行先を既定生成する(ADR-8: プロジェクトごと命名)。
// 解決できない(owner 不明など)場合は空(schema 上 target は任意)。
func (r *Resolver) channelTarget(name string, in File, owner, project string) string {
	switch name {
	case "homebrew":
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
		if owner != "" {
			return owner + "/" + project
		}
	case "goinstall":
		if in.Goinstall != nil && in.Goinstall.Module != "" {
			return in.Goinstall.Module
		}
		if mod, err := r.ModulePath(r.Root); err == nil && mod != "" {
			return mod
		}
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

func gitOriginURL(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
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
		return nil, err
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

// detectLicense は LICENSE ファイルから SPDX を粗く推定する。不確実なら空(07)。
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
