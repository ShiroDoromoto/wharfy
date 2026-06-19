package channel

import (
	"context"
	"fmt"
	"net/http"
)

// winget_github.go — Submitter の実体。microsoft/winget-pkgs を fork し、ブランチに manifest を
// commit して中央リポジトリへ PR を出す(設計 11A)。低レベルの fork/branch/commit/PR は
// ghPR(github_pr.go)に委譲する(winget / *-core 共通)。マージはしない。
//
// 注: 実 PR を Microsoft のリポジトリに出すため自動テストでは検証しない(fake Submitter で
// orchestration を検証する)。実運用でのみ走る経路。

// GitHubWingetSubmitter は GitHub API 経由の Submitter。
type GitHubWingetSubmitter struct {
	Token      string
	Central    string // 既定 microsoft/winget-pkgs
	BaseBranch string // 既定 master
	API        string // 既定 https://api.github.com
	HTTP       *http.Client
}

func NewGitHubWingetSubmitter(token string) *GitHubWingetSubmitter {
	return &GitHubWingetSubmitter{Token: token, Central: "microsoft/winget-pkgs", BaseBranch: "master",
		API: "https://api.github.com", HTTP: http.DefaultClient}
}

func (s *GitHubWingetSubmitter) central() string {
	if s.Central == "" {
		return "microsoft/winget-pkgs"
	}
	return s.Central
}

func (s *GitHubWingetSubmitter) base() string {
	if s.BaseBranch == "" {
		return "master"
	}
	return s.BaseBranch
}

func (s *GitHubWingetSubmitter) Submit(ctx context.Context, in WingetInput, files map[string]string) (string, error) {
	full := make(map[string]string, len(files))
	for name, content := range files {
		full[in.ManifestDir()+name] = content
	}
	g := &ghPR{Token: s.Token, API: s.API, HTTP: s.HTTP}
	title := fmt.Sprintf("New version: %s version %s", in.Identifier, in.Version)
	body := "Submitted by wharfy. Manifest for " + in.Identifier + " " + in.Version + "."
	return g.submit(ctx, s.central(), s.base(), in.BranchName(), full, "wharfy: winget manifest", title, body)
}
