package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// goreleaser.go — 実効設定から GoReleaser 設定を生成する(設計 03 / ADR-1・ADR-5)。
//
// wharfy が所有する配布物。利用者リポジトリ root には書かず `.wharfy/` キャッシュに置く(03)。
// version/commit/date は ldflags で自動注入する(tag が唯一の真実・samples/version.go)。
//
// スライス1 は homebrew 1 本＋releases まで。scoop/apt/rpm 等の横展開と passthrough の
// deep merge は後段(型を homebrew で固めてから・08 §5)。

// WharfyDirName は生成物・状態を置くスクラッチ。利用者のソースではないので所有してよい(03)。
const WharfyDirName = ".wharfy"

// GoReleaserFileName は生成する設定ファイル名(.wharfy/ 配下)。
const GoReleaserFileName = "goreleaser.yaml"

// --- 生成する GoReleaser 設定の最小サブセット(YAML 直列化用) ---

type glConfig struct {
	Version     int         `yaml:"version"`
	ProjectName string      `yaml:"project_name"`
	Dist        string      `yaml:"dist"`
	Builds      []glBuild   `yaml:"builds"`
	Archives    []glArchive `yaml:"archives"`
	NFPMs       []glNFPM    `yaml:"nfpms,omitempty"`
	Release     *glRelease  `yaml:"release,omitempty"`

	DockersV2 []glDockerV2 `yaml:"dockers_v2,omitempty"`
}

// glDockerV2 は OCI マルチアーキイメージ(11B)。per-arch ビルド＋manifest list を 1 ブロックに
// 統合した goreleaser の新形式(旧 dockers + docker_manifests を置換)。buildx で複数 platform
// を一度にビルドし、images×tags に push する。
type glDockerV2 struct {
	ID         string   `yaml:"id"`
	Images     []string `yaml:"images"`
	Tags       []string `yaml:"tags"`
	Platforms  []string `yaml:"platforms"`
	Dockerfile string   `yaml:"dockerfile"`
}

// glNFPM は deb/rpm 生成(apt/rpm チャネル)。nfpm 経由でビルド成果物からパッケージを作る。
type glNFPM struct {
	ID          string   `yaml:"id"`
	PackageName string   `yaml:"package_name"`
	IDs         []string `yaml:"ids"` // 含めるビルド id(旧 builds)
	Homepage    string   `yaml:"homepage,omitempty"`
	Description string   `yaml:"description,omitempty"`
	License     string   `yaml:"license,omitempty"`
	Maintainer  string   `yaml:"maintainer"`
	Formats     []string `yaml:"formats"`
}

// DistDir はビルド成果物の出力先(.wharfy 配下＝利用者 root を汚さない・03)。
// 生成設定の dist と Builder の参照先を一致させる単一定数。
const DistDir = WharfyDirName + "/dist"

type glBuild struct {
	ID      string   `yaml:"id"`
	Main    string   `yaml:"main"`
	Binary  string   `yaml:"binary"`
	Env     []string `yaml:"env,omitempty"`
	GOOS    []string `yaml:"goos"`
	GOARCH  []string `yaml:"goarch"`
	Ldflags []string `yaml:"ldflags"`
}

type glArchive struct {
	ID              string             `yaml:"id"`
	IDs             []string           `yaml:"ids"` // 含めるビルド id(旧 builds)
	NameTemplate    string             `yaml:"name_template"`
	FormatOverrides []glFormatOverride `yaml:"format_overrides,omitempty"`
}

type glFormatOverride struct {
	GOOS    string   `yaml:"goos"`
	Formats []string `yaml:"formats"` // 旧 format(単数)
}

type glRelease struct {
	GitHub     glRepository  `yaml:"github"`
	ExtraFiles []glExtraFile `yaml:"extra_files,omitempty"`
}

// glExtraFile は release に同梱アップロードする追加アセット(script の install.sh 等)。
type glExtraFile struct {
	Glob string `yaml:"glob"`
}

type glRepository struct {
	Owner string `yaml:"owner"`
	Name  string `yaml:"name"`
}

// GenerateGoReleaser は実効設定 cfg と、契約に出ない追加ビルド設定 in(env/ldflags_extra/
// homebrew.dependencies)から GoReleaser 設定 YAML を組み立てる。
//
// cfg は config.json で凍結した公開サブセット、in は生成のための非公開入力。両方を使う。
func GenerateGoReleaser(cfg Config, in File) ([]byte, error) {
	if cfg.Main == "" {
		// main 未解決のまま生成はしない(曖昧は config 段階で停止しているはず)。
		return nil, fmt.Errorf("cannot generate goreleaser config: 'main' is unresolved")
	}
	binary := firstNonEmpty(in.Binary, cfg.Project)

	gl := glConfig{
		Version:     2,
		ProjectName: cfg.Project,
		Dist:        DistDir,
		Builds: []glBuild{{
			ID:      cfg.Project,
			Main:    cfg.Main,
			Binary:  binary,
			Env:     buildEnv(in),
			GOOS:    buildGOOS(cfg),
			GOARCH:  buildGOARCH(cfg),
			Ldflags: ldflags(in),
		}},
		Archives: []glArchive{{
			ID:           cfg.Project,
			IDs:          []string{cfg.Project},
			NameTemplate: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}",
			FormatOverrides: []glFormatOverride{
				{GOOS: "windows", Formats: []string{"zip"}},
			},
		}},
	}

	// homebrew formula は wharfy が所有して直接書く(channel.GenerateFormula＋TapStore)。
	// goreleaser の brews は deprecated かつ常に --skip=homebrew で未使用なので生成しない。

	// nfpms: apt(deb)/rpm が有効な時だけ。1 エントリで該当 formats を生成する。
	if formats := pkgFormats(cfg); len(formats) > 0 {
		gl.NFPMs = []glNFPM{{
			ID:          cfg.Project,
			PackageName: cfg.Project,
			IDs:         []string{cfg.Project},
			Homepage:    cfg.Homepage,
			Description: in.Description,
			License:     cfg.License,
			Maintainer:  maintainer(cfg),
			Formats:     formats,
		}}
	}

	// release: github(owner/repo)が解決できる時だけ。最終フォールバック列(03)。
	// script チャネルは install.sh を同じ release に extra_files で同梱するため release を要する。
	if HasChannel(cfg, "releases") || HasChannel(cfg, "script") {
		if owner, name, ok := splitOwnerRepo(cfg.Github); ok {
			rel := &glRelease{GitHub: glRepository{Owner: owner, Name: name}}
			if HasChannel(cfg, "script") {
				rel.ExtraFiles = []glExtraFile{{Glob: InstallScriptRelPath}}
			}
			gl.Release = rel
		}
	}

	// container: ghcr のマルチアーキ OCI(linux amd64/arm64)。dockers_v2 1 ブロックで出す(11B)。
	if HasChannel(cfg, "container") {
		if image := containerImage(cfg); image != "" {
			gl.DockersV2 = []glDockerV2{dockerV2Block(cfg, image)}
		}
	}

	return marshalGoReleaser(gl)
}

// dockerV2Block は image に対する dockers_v2 エントリ(全 platform ＋ :version/:latest)を組む。
func dockerV2Block(cfg Config, image string) glDockerV2 {
	arches := []string{"amd64", "arm64"}
	if cfg.Build != nil && len(cfg.Build.GOARCH) > 0 {
		arches = cfg.Build.GOARCH
	}
	platforms := make([]string, 0, len(arches))
	for _, arch := range arches {
		platforms = append(platforms, "linux/"+arch)
	}
	return glDockerV2{
		ID:         cfg.Project,
		Images:     []string{image},
		Tags:       []string{"{{ .Version }}", "latest"},
		Platforms:  platforms,
		Dockerfile: DockerfileRelPath,
	}
}

// containerImage は container の解決済みイメージ名(channels の target)。
func containerImage(cfg Config) string {
	for _, ch := range cfg.Channels {
		if ch.Name == "container" {
			return ch.Target
		}
	}
	return ""
}

// marshalGoReleaser は生成物に「自動生成・編集しない」ヘッダを添えて YAML 化する。
func marshalGoReleaser(gl glConfig) ([]byte, error) {
	body, err := yaml.Marshal(gl)
	if err != nil {
		return nil, err
	}
	header := "# GENERATED BY wharfy — do not edit. Source of truth is wharfy.yaml.\n" +
		"# Owned distribution artifact (03_nondestructive_boundary). Lives under .wharfy/.\n"
	return append([]byte(header), body...), nil
}

// WriteGoReleaser は生成設定を <root>/.wharfy/goreleaser.yaml に書く。
// 利用者リポジトリ root には決して書かない(03 非破壊境界)。書いたパスを返す。
func WriteGoReleaser(root string, data []byte) (string, error) {
	dir := filepath.Join(root, WharfyDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, GoReleaserFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// GitignoreNeedsWharfy は .wharfy/ がまだ .gitignore に無いかを返す(true=案内すべき)。
// wharfy は .gitignore を**書かない**。next: で追記を提案するための判定だけ行う(03・冪等)。
func GitignoreNeedsWharfy(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return true // 無ければ案内する価値がある
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch strings.TrimSpace(line) {
		case ".wharfy/", ".wharfy", "/.wharfy/", "/.wharfy":
			return false
		}
	}
	return true
}

// --- ヘルパ ---

func buildEnv(in File) []string {
	if in.Build != nil && len(in.Build.Env) > 0 {
		return in.Build.Env
	}
	return []string{"CGO_ENABLED=0"}
}

func buildGOOS(cfg Config) []string {
	if cfg.Build != nil && len(cfg.Build.GOOS) > 0 {
		return cfg.Build.GOOS
	}
	return DefaultGOOS
}

func buildGOARCH(cfg Config) []string {
	if cfg.Build != nil && len(cfg.Build.GOARCH) > 0 {
		return cfg.Build.GOARCH
	}
	return DefaultGOARCH
}

// ldflags は version 注入(tag=唯一の真実)を必ず含め、利用者の追加分を後ろに足す。
func ldflags(in File) []string {
	base := []string{
		"-s -w",
		"-X main.version={{.Version}}",
		"-X main.commit={{.Commit}}",
		"-X main.date={{.Date}}",
	}
	if in.Build != nil {
		base = append(base, in.Build.LdflagsExtra...)
	}
	return base
}

// pkgFormats は有効な linux パッケージ形式(apt→deb / rpm→rpm)を返す。
func pkgFormats(cfg Config) []string {
	var f []string
	if HasChannel(cfg, "apt") {
		f = append(f, "deb")
	}
	if HasChannel(cfg, "rpm") {
		f = append(f, "rpm")
	}
	return f
}

// maintainer は nfpm が要求する maintainer を github owner から組む(deb は必須)。
func maintainer(cfg Config) string {
	if owner, _, ok := splitOwnerRepo(cfg.Github); ok {
		return fmt.Sprintf("%s <%s@users.noreply.github.com>", owner, owner)
	}
	return "wharfy <noreply@wharfy.local>"
}

func splitOwnerRepo(s string) (owner, name string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
