package channel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// script.go — script チャネルの実体照合(設計 04)。Release に同梱した install.sh を取得し、
// それが指す版(VERSION="x")を読む。status の記録 vs 実体の照合に使う。
// install.sh の生成は config パッケージ(所有する生成物)、ここは「実体の読み手」。

// Script は install.sh の実体を読む Probe 専用の型。
type Script struct {
	InstallURL string // 公開 install.sh の URL(releases/latest/download/install.sh)
	HTTP       *http.Client
}

var scriptVersionRe = regexp.MustCompile(`(?m)^VERSION="([^"]+)"`)

// ScriptVersion は install.sh 本文から VERSION を読む。
func ScriptVersion(content string) string {
	m := scriptVersionRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return m[1]
}

// Probe は install.sh を取得して版を返す(404/未公開は found=false)。
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
	return RemoteState{Version: ScriptVersion(string(body)), Found: true}, nil
}

func (s *Script) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}
