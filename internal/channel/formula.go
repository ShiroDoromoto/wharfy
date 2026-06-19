package channel

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// formula.go — Homebrew formula(.rb)を生成する(設計 02 出力契約・examples/formula.example.rb)。
// wharfy が所有する配布物。形は安定・機械生成で、利用者は直接書かない(03)。
// macOS は未署名だと cask が Gatekeeper に弾かれるため formula を採用する。

// ArchiveRef は 1 つの配布アーカイブ(os/arch ごと)。URL は Releases のダウンロード先。
type ArchiveRef struct {
	OS     string // darwin | linux
	Arch   string // amd64 | arm64
	URL    string
	SHA256 string
}

// FormulaInput は formula 生成の入力(解決済み設定＋アーカイブ情報から組む)。
type FormulaInput struct {
	Project     string
	Binary      string // 既定: Project
	Description string
	Homepage    string
	License     string
	Version     string // 先頭 v なしの版(例: 1.4.0)
	Archives    []ArchiveRef
}

// GenerateFormula は formula 文字列を生成する。darwin/linux の arm/intel を持つ分だけ出す。
func GenerateFormula(in FormulaInput) string {
	binary := in.Binary
	if binary == "" {
		binary = in.Project
	}
	var b strings.Builder
	fmt.Fprintf(&b, "class %s < Formula\n", className(in.Project))
	if in.Description != "" {
		fmt.Fprintf(&b, "  desc %q\n", in.Description)
	}
	if in.Homepage != "" {
		fmt.Fprintf(&b, "  homepage %q\n", in.Homepage)
	}
	fmt.Fprintf(&b, "  version %q\n", in.Version)
	if in.License != "" {
		fmt.Fprintf(&b, "  license %q\n", in.License)
	}
	b.WriteString("\n")

	writeOSBlock(&b, "on_macos", "darwin", in.Archives)
	writeOSBlock(&b, "on_linux", "linux", in.Archives)

	fmt.Fprintf(&b, "  def install\n    bin.install %q\n  end\n\n", binary)
	fmt.Fprintf(&b, "  test do\n    assert_match %q, shell_output(\"#{bin}/%s version\")\n  end\n", in.Version, binary)
	b.WriteString("end\n")
	return b.String()
}

// writeOSBlock は 1 OS の on_arm / on_intel ブロックを、該当 arch がある時だけ書く。
func writeOSBlock(b *strings.Builder, block, os string, archives []ArchiveRef) {
	arm := findArchive(archives, os, "arm64")
	intel := findArchive(archives, os, "amd64")
	if arm == nil && intel == nil {
		return
	}
	fmt.Fprintf(b, "  %s do\n", block)
	if arm != nil {
		writeArchBlock(b, "on_arm", *arm)
	}
	if intel != nil {
		writeArchBlock(b, "on_intel", *intel)
	}
	b.WriteString("  end\n\n")
}

func writeArchBlock(b *strings.Builder, block string, a ArchiveRef) {
	fmt.Fprintf(b, "    %s do\n", block)
	fmt.Fprintf(b, "      url %q\n", a.URL)
	fmt.Fprintf(b, "      sha256 %q\n", a.SHA256)
	b.WriteString("    end\n")
}

func findArchive(archives []ArchiveRef, os, arch string) *ArchiveRef {
	for i := range archives {
		if archives[i].OS == os && archives[i].Arch == arch {
			return &archives[i]
		}
	}
	return nil
}

var versionRe = regexp.MustCompile(`(?m)^\s*version\s+"([^"]+)"`)

// FormulaVersion は formula 文字列から version を読む(Probe の版照合に使う)。
func FormulaVersion(content string) string {
	m := versionRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return m[1]
}

// className は project 名を Homebrew の CamelCase クラス名にする(- _ で区切って連結)。
func className(project string) string {
	parts := strings.FieldsFunc(project, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// SortArchives は os/arch で安定順にする(生成 formula を決定的にして diff/golden を安定させる)。
func SortArchives(archives []ArchiveRef) {
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].OS != archives[j].OS {
			return archives[i].OS < archives[j].OS
		}
		return archives[i].Arch < archives[j].Arch
	})
}
