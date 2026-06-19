package channel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// oci.go — container(ghcr)の実体照合(設計 04・11B)。Docker Registry HTTP API V2 で
// イメージのタグ存在を確認する。version タグが在れば found(=配布済み)。
// 公開イメージは匿名トークン、非公開は GITHUB_TOKEN で token 交換する。

// OCIProbe は OCI レジストリ(ghcr)へのタグ存在 probe。
type OCIProbe struct {
	Image string // 例 ghcr.io/acme/widget
	Token string // GITHUB_TOKEN(空なら匿名)
	Base  string // テスト用にレジストリ host を差し替え(空＝image のホスト)
	HTTP  *http.Client
}

func (p *OCIProbe) client() *http.Client {
	if p.HTTP == nil {
		return http.DefaultClient
	}
	return p.HTTP
}

// registryHost / registryRepo は image を host と repo に分ける(ghcr.io/acme/widget → host, acme/widget)。
func registryHost(image string) string {
	if i := strings.IndexByte(image, '/'); i > 0 {
		return image[:i]
	}
	return image
}

func registryRepo(image string) string {
	if i := strings.IndexByte(image, '/'); i > 0 {
		return image[i+1:]
	}
	return image
}

func (p *OCIProbe) base() string {
	if p.Base != "" {
		return strings.TrimRight(p.Base, "/")
	}
	return "https://" + registryHost(p.Image)
}

// Probe は image:version のタグが在るかを確認する(在れば found・version 一致扱い)。
func (p *OCIProbe) Probe(ctx context.Context, version string) (RemoteState, error) {
	repo := registryRepo(p.Image)
	token, err := p.authToken(ctx, repo)
	if err != nil {
		return RemoteState{}, err
	}
	url := p.base() + "/v2/" + repo + "/manifests/" + version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RemoteState{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := p.client().Do(req)
	if err != nil {
		return RemoteState{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return RemoteState{Version: version, Found: true}, nil
	case http.StatusNotFound:
		return RemoteState{Found: false}, nil
	default:
		return RemoteState{}, fmt.Errorf("registry %s: %s", url, resp.Status)
	}
}

// authToken はレジストリの token エンドポイントから pull トークンを得る(匿名 or GITHUB_TOKEN)。
func (p *OCIProbe) authToken(ctx context.Context, repo string) (string, error) {
	host := registryHost(p.Image)
	url := fmt.Sprintf("%s/token?service=%s&scope=repository:%s:pull", p.base(), host, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if p.Token != "" {
		req.SetBasicAuth("wharfy", p.Token)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// token 取得不可でも匿名アクセスが通ることがある。トークン無しで続行。
		return "", nil
	}
	// {"token":"..."} または {"access_token":"..."}。
	tok := jsonField(string(body), "token")
	if tok == "" {
		tok = jsonField(string(body), "access_token")
	}
	return tok, nil
}

// jsonField は素朴に "key":"value" を抜く(token 応答用の最小実装。依存を増やさない)。
func jsonField(s, key string) string {
	needle := "\"" + key + "\""
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	rest := s[i+len(needle):]
	c := strings.IndexByte(rest, ':')
	if c < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[c+1:])
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
