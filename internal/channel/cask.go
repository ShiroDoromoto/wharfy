package channel

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// cask.go — Homebrew Cask(.rb)を生成する(GUI バンドル配布・依頼②)。
// Formula(CLI)と同じ tap に同居させ(Casks/<token>.rb)、`wharfy status` で一元表示する(依頼④)。
// 形は安定・機械生成で、利用者は直接書かない(03)。formula.go の対。
//
// バンドルは各アプリが署名済みで持ち込む(BYO-bundle)。wharfy は再署名しない — 非 notarized なら
// macOS の quarantine で初回起動時に Gatekeeper 警告が出るため、caveats に回避手順を書く(依頼⑤)。

// CaskArtifact は 1 つの配布バンドル(arch ごと)。URL は Release のダウンロード先。
type CaskArtifact struct {
	Arch   string // arm64 | amd64 | universal
	URL    string
	SHA256 string
}

// CaskInput は cask 生成の入力(解決済み設定＋バンドル情報から組む)。
//   - Token: cask 識別子(`cask "<token>"` とファイル名)。既定は publish 側で <project>-app に解決。
//   - Name: 表示名 "<App>"(name stanza)。token とは独立・不変(依頼書 §4)。
//   - AppBundle: app stanza の対象("<App>.app")。空なら "<Name>.app"。
//   - Notarized: false(既定)なら Gatekeeper 案内の caveats を出す(依頼⑤)。
type CaskInput struct {
	Token     string
	Name      string
	Desc      string
	Homepage  string
	Version   string // 先頭 v なしの版(例: 1.4.0)
	AppBundle string
	Notarized bool
	Artifacts []CaskArtifact
}

// GenerateCask は cask 文字列を生成する。universal 単独なら top-level url、arm/intel を持つなら
// on_arm/on_intel ブロックに分ける(formula の on_macos 相当を macOS 単一 OS で表現)。
func GenerateCask(in CaskInput) string {
	name := in.Name
	if name == "" {
		name = in.Token
	}
	appBundle := in.AppBundle
	if appBundle == "" {
		appBundle = name + ".app"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cask %q do\n", in.Token)
	fmt.Fprintf(&b, "  version %q\n", in.Version)
	writeCaskURLs(&b, in.Artifacts)
	b.WriteString("\n")
	fmt.Fprintf(&b, "  name %q\n", name)
	if in.Desc != "" {
		fmt.Fprintf(&b, "  desc %q\n", in.Desc)
	}
	if in.Homepage != "" {
		fmt.Fprintf(&b, "  homepage %q\n", in.Homepage)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  app %q\n", appBundle)
	if !in.Notarized {
		b.WriteString("\n")
		writeCaskCaveats(&b, name)
	}
	b.WriteString("end\n")
	return b.String()
}

// writeCaskURLs は arch ごとの url/sha256 を書く。universal だけなら top-level に 1 組、
// arm/intel があればそれぞれ on_arm/on_intel ブロックに出す(決定的に sort 済み前提)。
func writeCaskURLs(b *strings.Builder, arts []CaskArtifact) {
	uni := findCaskArtifact(arts, "universal")
	arm := findCaskArtifact(arts, "arm64")
	intel := findCaskArtifact(arts, "amd64")
	if uni != nil && arm == nil && intel == nil {
		fmt.Fprintf(b, "  url %q\n", uni.URL)
		fmt.Fprintf(b, "  sha256 %q\n", uni.SHA256)
		return
	}
	if arm != nil {
		writeCaskArch(b, "on_arm", *arm)
	}
	if intel != nil {
		writeCaskArch(b, "on_intel", *intel)
	}
}

func writeCaskArch(b *strings.Builder, block string, a CaskArtifact) {
	fmt.Fprintf(b, "  %s do\n", block)
	fmt.Fprintf(b, "    url %q\n", a.URL)
	fmt.Fprintf(b, "    sha256 %q\n", a.SHA256)
	b.WriteString("  end\n")
}

// writeCaskCaveats は非 notarized バンドルの Gatekeeper 回避手順を案内する(依頼⑤)。
func writeCaskCaveats(b *strings.Builder, name string) {
	b.WriteString("  caveats <<~EOS\n")
	fmt.Fprintf(b, "    %s is self-signed and not notarized. On first launch macOS Gatekeeper\n", name)
	fmt.Fprintf(b, "    may block it. Right-click %s in Finder and choose Open to run it.\n", name)
	b.WriteString("  EOS\n")
}

func findCaskArtifact(arts []CaskArtifact, arch string) *CaskArtifact {
	for i := range arts {
		if arts[i].Arch == arch {
			return &arts[i]
		}
	}
	return nil
}

var caskVersionRe = regexp.MustCompile(`(?m)^\s*version\s+"([^"]+)"`)

// CaskVersion は cask 文字列から version を読む(Probe の版照合に使う)。
func CaskVersion(content string) string {
	m := caskVersionRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return m[1]
}

// SortCaskArtifacts は arch で安定順にする(生成 cask を決定的にして diff/golden を安定させる)。
func SortCaskArtifacts(arts []CaskArtifact) {
	sort.Slice(arts, func(i, j int) bool { return arts[i].Arch < arts[j].Arch })
}
