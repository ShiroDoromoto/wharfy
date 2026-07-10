package channel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// script.go — script チャネルの実体照合。Release に同梱したインストーラを取得し、それが指す版を読む
// (install.sh は VERSION="x"、install.ps1 は $Version = 'x')。status の記録 vs 実体の照合に使う。
// 生成は config パッケージ(所有する生成物)、ここは「実体の読み手」。版をどこに書くかは両者の約束。

// Script は公開インストーラの実体を読む Probe 専用の型。PS1 を立てると本文を install.ps1 として
// 読む —— install.sh と install.ps1 は同じ版を別の書式で書くので、読み手を取り違えると版が空になる。
type Script struct {
	InstallURL string // 公開インストーラの URL(releases/latest/download/install.sh など)
	PS1        bool   // 本文は install.ps1(既定は install.sh)
	HTTP       *http.Client
}

var (
	scriptVersionRe = regexp.MustCompile(`(?m)^VERSION="([^"]+)"`)
	ps1VersionRe    = regexp.MustCompile(`(?m)^\$Version\s*=\s*'([^']+)'`)
)

// ScriptVersion は install.sh 本文から VERSION を読む。
func ScriptVersion(content string) string {
	return firstSubmatch(scriptVersionRe, content)
}

// PS1Version は install.ps1 本文から $Version を読む(config.GenerateInstallPS1 が書く行)。
func PS1Version(content string) string {
	return firstSubmatch(ps1VersionRe, content)
}

func firstSubmatch(re *regexp.Regexp, content string) string {
	m := re.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return m[1]
}

// Probe はインストーラを取得して版を返す(404/未公開は found=false)。
func (s *Script) Probe(ctx context.Context) (RemoteState, error) {
	if s.InstallURL == "" {
		return RemoteState{Found: false}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.InstallURL, nil)
	if err != nil {
		return RemoteState{}, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return RemoteState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return RemoteState{Found: false}, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return RemoteState{}, fmt.Errorf("fetch %s: %s", s.InstallURL, resp.Status)
	}
	return RemoteState{Version: s.version(string(body)), Found: true}, nil
}

func (s *Script) version(body string) string {
	if s.PS1 {
		return PS1Version(body)
	}
	return ScriptVersion(body)
}

func (s *Script) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}
