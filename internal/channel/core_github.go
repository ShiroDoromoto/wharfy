package channel

import (
	"context"
	"net/http"
)

// core_github.go — CoreSubmitter の実体。上流(Homebrew/homebrew-core 等)を fork し、formula を
// commit して PR を出す。低レベルは ghPR に委譲(winget と共通)。マージはしない。
// 実 PR を上流に出すため自動テストでは検証しない(fake で orchestration を検証)。

// GitHubCoreSubmitter は GitHub API 経由の CoreSubmitter。
type GitHubCoreSubmitter struct {
	Token      string
	BaseBranch string // 既定 master(homebrew-core)
	API        string // 既定 https://api.github.com
	HTTP       *http.Client
}

func NewGitHubCoreSubmitter(token string) *GitHubCoreSubmitter {
	return &GitHubCoreSubmitter{Token: token, BaseBranch: "master", API: "https://api.github.com", HTTP: http.DefaultClient}
}

func (s *GitHubCoreSubmitter) Submit(ctx context.Context, in CoreInput) (string, error) {
	base := s.BaseBranch
	if base == "" {
		base = "master"
	}
	g := &ghPR{Token: s.Token, API: s.API, HTTP: s.HTTP}
	files := map[string]string{in.FormulaFile: in.Formula}
	title := in.Project + " " + in.Version
	body := "Submitted by wharfy. Formula for " + in.Project + " " + in.Version + "."
	return g.submit(ctx, in.Central, base, in.BranchName(), files, "wharfy: "+in.Project+" "+in.Version, title, body)
}
