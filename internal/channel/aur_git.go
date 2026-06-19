package channel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// aur_git.go — AurPusher の実体。AUR の自前パッケージ git(ssh)を clone し、PKGBUILD/.SRCINFO を
// commit して push する(設計 03・審査なし)。AUR_SSH_KEY を一時ファイルに置き GIT_SSH_COMMAND で使う。
//
// 注: 実 AUR へ push するため自動テストでは検証しない(fake AurPusher で orchestration を検証)。

// GitAurPusher は git+ssh で AUR に push する。
type GitAurPusher struct {
	SSHKey  string // AUR_SSH_KEY(秘密鍵本文)
	GitName string
	GitMail string
	Host    string // 既定 aur@aur.archlinux.org
}

func NewGitAurPusher(sshKey string) *GitAurPusher {
	return &GitAurPusher{SSHKey: sshKey, GitName: "wharfy", GitMail: "wharfy@users.noreply.github.com", Host: "aur@aur.archlinux.org"}
}

func (p *GitAurPusher) Push(ctx context.Context, pkgname string, files map[string]string) (string, error) {
	if p.SSHKey == "" {
		return "", fmt.Errorf("AUR_SSH_KEY required to push to AUR")
	}
	work, err := os.MkdirTemp("", "wharfy-aur-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	// 秘密鍵を一時ファイルに置き、GIT_SSH_COMMAND で使う。
	keyPath := filepath.Join(work, "aur_key")
	if err := os.WriteFile(keyPath, []byte(ensureTrailingNewline(p.SSHKey)), 0o600); err != nil {
		return "", err
	}
	sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", keyPath)
	env := append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd,
		"GIT_AUTHOR_NAME="+p.GitName, "GIT_AUTHOR_EMAIL="+p.GitMail,
		"GIT_COMMITTER_NAME="+p.GitName, "GIT_COMMITTER_EMAIL="+p.GitMail)

	repo := filepath.Join(work, pkgname)
	remote := p.host() + "/" + pkgname + ".git"
	if out, err := p.git(ctx, env, work, "clone", remote, repo); err != nil {
		return "", fmt.Errorf("clone %s: %w: %s", remote, err, out)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	if out, err := p.git(ctx, env, repo, "add", "PKGBUILD", ".SRCINFO"); err != nil {
		return "", fmt.Errorf("add: %w: %s", err, out)
	}
	if out, err := p.git(ctx, env, repo, "commit", "-m", "wharfy: update "+pkgname); err != nil {
		// 変更なし(冪等)なら commit は失敗する。push せず空 commit を返す。
		return "", fmt.Errorf("commit (maybe no changes): %w: %s", err, out)
	}
	if out, err := p.git(ctx, env, repo, "push", "origin", "HEAD:master"); err != nil {
		return "", fmt.Errorf("push: %w: %s", err, out)
	}
	out, _ := p.git(ctx, env, repo, "rev-parse", "HEAD")
	return trimSpace(out), nil
}

func (p *GitAurPusher) host() string {
	h := p.Host
	if h == "" {
		h = "aur@aur.archlinux.org"
	}
	return "ssh://" + h
}

func (p *GitAurPusher) git(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ensureTrailingNewline(s string) string {
	if len(s) == 0 || s[len(s)-1] != '\n' {
		return s + "\n"
	}
	return s
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
