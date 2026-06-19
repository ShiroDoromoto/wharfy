package channel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// goinstall.go — go install チャネル(owned・梱包ゼロ。設計 01/03/07)。
//
// 発行物を push しない特殊な owned: module が `go install` 可能なこと＋version 注入を
// 確認して手順を案内するだけ(Plan/Publish は実質 noop、Probe で版を確認)。
// version 注入は ldflags 不要で、go install 時に debug.ReadBuildInfo(module version)で効く
// (samples/version.go の fallback)。

// GoInstall は goinstall チャネルの Publisher。
type GoInstall struct {
	Module      string // module proxy 照合用の root module path(例: github.com/o/r)
	InstallPath string // go install 対象(例: github.com/o/r/cmd/x)。既定は Module
	Version     string // 先頭 v 付きのタグ(例: v0.1.0)
	Proxy       string // 既定 https://proxy.golang.org
	HTTP        *http.Client
}

func (g *GoInstall) Name() string { return "goinstall" }
func (g *GoInstall) Kind() string { return KindOwned }

// InstallCommand は利用者に案内する go install コマンド。
func (g *GoInstall) InstallCommand() string {
	path := g.InstallPath
	if path == "" {
		path = g.Module
	}
	if g.Version == "" {
		return "go install " + path + "@latest"
	}
	return "go install " + path + "@" + g.Version
}

// Plan は梱包ゼロ＝書くものが無いので常に noop(owned_artifact も持たない)。
func (g *GoInstall) Plan(_ context.Context) (PlanItem, error) {
	return PlanItem{
		Channel: g.Name(), Kind: g.Kind(), Action: ActionNoop,
		Reason: "go install needs no artifact (zero packaging); ensure the tag is pushed and the repo is public",
	}, nil
}

// Publish も何も push しない。到達性は Probe で確認する。
func (g *GoInstall) Publish(ctx context.Context) (PlanItem, PubResult, error) {
	item, err := g.Plan(ctx)
	return item, PubResult{}, err
}

// Probe は module proxy に版があるか(=go install できるか)を確認する(04 の実体照合)。
func (g *GoInstall) Probe(ctx context.Context) (RemoteState, error) {
	if g.Version == "" {
		return RemoteState{Found: false}, nil
	}
	base := g.Proxy
	if base == "" {
		base = "https://proxy.golang.org"
	}
	url := fmt.Sprintf("%s/%s/@v/%s.info", strings.TrimRight(base, "/"), escapeModule(g.Module), g.Version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RemoteState{}, err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return RemoteState{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return RemoteState{Version: g.Version, Found: true}, nil
	case http.StatusNotFound, http.StatusGone:
		return RemoteState{Found: false}, nil
	default:
		return RemoteState{}, fmt.Errorf("module proxy %s: %s", url, resp.Status)
	}
}

func (g *GoInstall) client() *http.Client {
	if g.HTTP == nil {
		return http.DefaultClient
	}
	return g.HTTP
}

// escapeModule は module path を Go module proxy の規約でエスケープする
// (大文字は ! + 小文字。例: github.com/Foo → github.com/!foo)。
func escapeModule(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
