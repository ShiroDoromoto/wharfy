package channel

import (
	"context"
	"fmt"
	"strings"
)

// core.go — *-core(中央キュレーション repo への gated 配布)。代表は homebrew-core。
// 自分の tap と違い上流のレビュー＆マージが要る(winget と同じ gated・11A)。wharfy は formula を
// 生成して fork PR を出し、状態を追うだけ。マージはしない。受け入れ基準(brew audit 等)は利用者責務。

// CoreInput は *-core への申請 1 件。
type CoreInput struct {
	Central     string // 上流 owner/repo(例: Homebrew/homebrew-core)
	Project     string
	Version     string
	FormulaFile string // 上流リポジトリでのパス(例: Formula/w/wharfy.rb)
	Formula     string // 生成済み formula の内容
}

// BranchName は申請ブランチ名(プロジェクト＋版で一意・冪等)。
func (in CoreInput) BranchName() string { return "wharfy-" + in.Project + "-" + in.Version }

// CoreFormulaPath は homebrew-core のシャード済みパス Formula/<先頭文字>/<name>.rb を返す。
func CoreFormulaPath(name string) string {
	n := strings.ToLower(name)
	letter := "_"
	if n != "" {
		letter = n[:1]
	}
	return "Formula/" + letter + "/" + n + ".rb"
}

// CoreSubmitter は *-core への gated PR 提出境界(テストで fake 化)。
type CoreSubmitter interface {
	Submit(ctx context.Context, in CoreInput) (prURL string, err error)
}

// CoreFormulaInput は homebrew-core 向け source-build formula の入力。
// binary を同梱せずソースから `go build` する core 流儀(自前 tap の binary formula とは別物)。
type CoreFormulaInput struct {
	Project     string
	Binary      string // 既定: Project
	Description string
	Homepage    string
	License     string
	Version     string // 先頭 v なし
	SourceURL   string // ソース tarball(GitHub の tags archive)
	SourceSHA   string // そのソース tarball の sha256(空なら dry-run の暫定表示)
}

// GenerateCoreFormula は homebrew-core 向けの source-build formula(Go)を生成する。
// あくまで叩き台で、`brew audit --new --strict` の合格保証ではない(11A)。
func GenerateCoreFormula(in CoreFormulaInput) string {
	binary := in.Binary
	if binary == "" {
		binary = in.Project
	}
	sha := in.SourceSHA
	if sha == "" {
		sha = "<computed on --yes>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "class %s < Formula\n", className(in.Project))
	if in.Description != "" {
		fmt.Fprintf(&b, "  desc %q\n", in.Description)
	}
	if in.Homepage != "" {
		fmt.Fprintf(&b, "  homepage %q\n", in.Homepage)
	}
	fmt.Fprintf(&b, "  url %q\n", in.SourceURL)
	fmt.Fprintf(&b, "  sha256 %q\n", sha)
	if in.License != "" {
		fmt.Fprintf(&b, "  license %q\n", in.License)
	}
	b.WriteString("\n  depends_on \"go\" => :build\n\n")
	b.WriteString("  def install\n")
	b.WriteString("    system \"go\", \"build\", *std_go_args(ldflags: \"-s -w -X main.version=#{version}\")\n")
	b.WriteString("  end\n\n")
	b.WriteString("  test do\n")
	fmt.Fprintf(&b, "    assert_match version.to_s, shell_output(\"#{bin}/%s version\")\n", binary)
	b.WriteString("  end\n")
	b.WriteString("end\n")
	return b.String()
}
