package channel

import (
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// linuxrepo.go — apt/rpm hosted repo の実体照合(設計 04)。best-effort: 慣習的なメタデータ
// (apt=Debian Packages / rpm=repomd→primary)を引いて版を読む。レイアウトが違えば取得できず
// status は recorded に落ちる(誤検出はしない)。base はテストで httptest に差し替え可能。

// AptProbe は Debian Packages から package の版を読む。
type AptProbe struct {
	Repo  string // apt.repo(archive root とみなす)
	Suite string // 既定 stable
	HTTP  *http.Client
}

var aptVersionRe = regexp.MustCompile(`(?m)^Version:\s*(\S+)`)

// Probe は <repo>/dists/<suite>/main/binary-amd64/Packages を引き、pkg の Version を返す。
func (p *AptProbe) Probe(ctx context.Context, pkg string) (RemoteState, error) {
	suite := p.Suite
	if suite == "" {
		suite = "stable"
	}
	url := strings.TrimRight(p.Repo, "/") + "/dists/" + suite + "/main/binary-amd64/Packages"
	body, ok, err := httpGet(ctx, p.HTTP, url)
	if err != nil {
		return RemoteState{}, err
	}
	if !ok {
		return RemoteState{Found: false}, nil
	}
	// Packages は空行区切りのスタンザ。Package: <pkg> のスタンザの Version を取る。
	for _, stanza := range strings.Split(string(body), "\n\n") {
		if !strings.Contains(stanza, "Package: "+pkg) {
			continue
		}
		if m := aptVersionRe.FindStringSubmatch(stanza); m != nil {
			return RemoteState{Version: stripPkgrel(m[1]), Found: true}, nil
		}
	}
	return RemoteState{Found: false}, nil
}

// RpmProbe は repomd.xml → primary.xml(.gz) を辿り、package の版を読む。
type RpmProbe struct {
	Repo string // rpm.repo(yum/dnf repo root)
	HTTP *http.Client
}

func (p *RpmProbe) Probe(ctx context.Context, pkg string) (RemoteState, error) {
	root := strings.TrimRight(p.Repo, "/")
	repomd, ok, err := httpGet(ctx, p.HTTP, root+"/repodata/repomd.xml")
	if err != nil {
		return RemoteState{}, err
	}
	if !ok {
		return RemoteState{Found: false}, nil
	}
	var md struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(repomd, &md); err != nil {
		return RemoteState{}, err
	}
	href := ""
	for _, d := range md.Data {
		if d.Type == "primary" {
			href = d.Location.Href
			break
		}
	}
	if href == "" {
		return RemoteState{Found: false}, nil
	}
	raw, ok, err := httpGet(ctx, p.HTTP, root+"/"+href)
	if err != nil {
		return RemoteState{}, err
	}
	if !ok {
		return RemoteState{Found: false}, nil
	}
	if strings.HasSuffix(href, ".gz") {
		if raw, err = gunzip(raw); err != nil {
			return RemoteState{}, err
		}
	}
	var primary struct {
		Packages []struct {
			Name    string `xml:"name"`
			Version struct {
				Ver string `xml:"ver,attr"`
			} `xml:"version"`
		} `xml:"package"`
	}
	if err := xml.Unmarshal(raw, &primary); err != nil {
		return RemoteState{}, err
	}
	for _, p := range primary.Packages {
		if p.Name == pkg {
			return RemoteState{Version: p.Version.Ver, Found: true}, nil
		}
	}
	return RemoteState{Found: false}, nil
}

// httpGet は GET し、200 の本文を返す。404/410 は ok=false(エラーではない)。
func httpGet(ctx context.Context, cl *http.Client, url string) ([]byte, bool, error) {
	if cl == nil {
		cl = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("get %s: %s", url, resp.Status)
	}
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
