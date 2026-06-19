package channel

import (
	"context"
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
