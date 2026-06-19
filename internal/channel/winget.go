package channel

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// winget.go — winget(gated)。中央リポジトリ microsoft/winget-pkgs へ申請する manifest を
// 生成する(設計 11A)。wharfy は「fork→branch→manifest→PR→状態追跡」までを担い、マージはしない。
// 生成するのは v1.6 の 3 種 manifest(version / installer / locale)。
//
// 実体(fork/PR)は Submitter(channel/winget_github.go)。ここは「申請物の生成」。

const wingetManifestVersion = "1.6.0"
const wingetLocale = "en-US"

// WingetInstaller は 1 アーキの windows zip(中に exe)。
type WingetInstaller struct {
	Arch   string // amd64 | arm64
	URL    string
	SHA256 string // 小文字 hex(生成時に大文字化する)
}

// WingetInput は manifest 生成の入力。
type WingetInput struct {
	Identifier  string // PackageIdentifier(例: ShiroDoromoto.widget)
	Project     string // バイナリ名(<project>.exe)
	Version     string
	License     string
	Description string
	Homepage    string
	Installers  []WingetInstaller
}

// publisher / pkg は Identifier を分解する(最初のドットで Publisher と Package)。
func (in WingetInput) publisher() string {
	if i := strings.IndexByte(in.Identifier, '.'); i > 0 {
		return in.Identifier[:i]
	}
	return in.Identifier
}

func (in WingetInput) pkg() string {
	if i := strings.IndexByte(in.Identifier, '.'); i >= 0 && i < len(in.Identifier)-1 {
		return in.Identifier[i+1:]
	}
	return in.Identifier
}

// ManifestDir は中央リポジトリ内の配置先(manifests/<l>/<Publisher>/<Package>/<version>/)。
func (in WingetInput) ManifestDir() string {
	segs := strings.Split(in.Identifier, ".")
	first := strings.ToLower(in.Identifier[:1])
	return "manifests/" + first + "/" + strings.Join(segs, "/") + "/" + in.Version + "/"
}

// BranchName は fork 上の作業ブランチ(冪等: 同 version は同名)。
func (in WingetInput) BranchName() string {
	return "wharfy/" + in.pkg() + "-" + in.Version
}

// wingetArch は goarch を winget の Architecture に直す(amd64→x64)。
func wingetArch(arch string) string {
	if arch == "amd64" {
		return "x64"
	}
	return arch
}

// GenerateWingetManifests は 3 種 manifest を {ファイル名: 内容} で返す(申請物)。
func GenerateWingetManifests(in WingetInput) map[string]string {
	id := in.Identifier
	header := "# yaml-language-server: $schema=https://aka.ms/winget-manifest.%s.%s.schema.json\n"

	version := map[string]any{
		"PackageIdentifier": id,
		"PackageVersion":    in.Version,
		"DefaultLocale":     wingetLocale,
		"ManifestType":      "version",
		"ManifestVersion":   wingetManifestVersion,
	}

	var installers []map[string]any
	for _, ins := range in.Installers {
		installers = append(installers, map[string]any{
			"Architecture":        wingetArch(ins.Arch),
			"InstallerType":       "zip",
			"InstallerUrl":        ins.URL,
			"InstallerSha256":     strings.ToUpper(ins.SHA256),
			"NestedInstallerType": "portable",
			"NestedInstallerFiles": []map[string]any{{
				"RelativeFilePath":     in.Project + ".exe",
				"PortableCommandAlias": in.Project,
			}},
		})
	}
	installer := map[string]any{
		"PackageIdentifier": id,
		"PackageVersion":    in.Version,
		"Installers":        installers,
		"ManifestType":      "installer",
		"ManifestVersion":   wingetManifestVersion,
	}

	locale := map[string]any{
		"PackageIdentifier": id,
		"PackageVersion":    in.Version,
		"PackageLocale":     wingetLocale,
		"Publisher":         in.publisher(),
		"PackageName":       in.pkg(),
		"License":           orNone(in.License),
		"ShortDescription":  orNone(in.Description),
		"ManifestType":      "defaultLocale",
		"ManifestVersion":   wingetManifestVersion,
	}
	if in.Homepage != "" {
		locale["PackageUrl"] = in.Homepage
	}

	return map[string]string{
		id + ".yaml":                             fmt.Sprintf(header, "version", "1.6.0") + marshalYAML(version),
		id + ".installer.yaml":                   fmt.Sprintf(header, "installer", "1.6.0") + marshalYAML(installer),
		id + ".locale." + wingetLocale + ".yaml": fmt.Sprintf(header, "defaultLocale", "1.6.0") + marshalYAML(locale),
	}
}

func marshalYAML(v any) string {
	b, _ := yaml.Marshal(v)
	return string(b)
}

func orNone(s string) string {
	if s == "" {
		return "NONE"
	}
	return s
}

// Submitter は gated 申請の実体(fork→branch→commit→PR)。実装は GitHub。テストは fake。
type Submitter interface {
	// Submit は申請物 files を fork のブランチに置き、中央リポジトリへ PR を出す。
	// 冪等: 同 version の PR が既にあれば既存を返す。PR の URL を返す。
	Submit(ctx context.Context, in WingetInput, files map[string]string) (prURL string, err error)
}
