package channel

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// linuxrepo.go — apt/rpm hosted repo の実体照合。best-effort: 慣習的なメタデータ
// (apt=Debian Packages / rpm=repomd→primary)を引いて版を読む。レイアウトが違えば取得できず
// status は recorded に落ちる(誤検出はしない)。base はテストで httptest に差し替え可能。

// AptProbe は Debian Packages から package の版を読む。
type AptProbe struct {
	Repo  string // apt.repo(archive root とみなす)
	Suite string // 既定 stable
	HTTP  *http.Client
}

var aptVersionRe = regexp.MustCompile(`(?m)^Version:\s*(\S+)`)

// Probe は apt メタデータ(Debian Packages)から pkg の最新版を返す。
// Gemfury 等の flat repo(<repo>/Packages)を優先し、無ければ正式レイアウト
// (<repo>/dists/<suite>/main/binary-amd64/Packages)にフォールバックする。
// flat repo は過去版も全て載るため、マッチするスタンザの中で最も高い版を返す。
func (p *AptProbe) Probe(ctx context.Context, pkg string) (RemoteState, error) {
	suite := p.Suite
	if suite == "" {
		suite = "stable"
	}
	root := strings.TrimRight(p.Repo, "/")
	var body []byte
	for _, url := range []string{
		root + "/Packages", // flat repo(Gemfury 等)
		root + "/dists/" + suite + "/main/binary-amd64/Packages", // 正式レイアウト
	} {
		b, ok, err := httpGet(ctx, p.HTTP, url)
		if err != nil {
			return RemoteState{}, err
		}
		if ok {
			body = b
			break
		}
	}
	if body == nil {
		return RemoteState{Found: false}, nil
	}
	// Packages は空行区切りのスタンザ。Package: <pkg> のスタンザの Version を集め、最新を返す。
	latest := ""
	for _, stanza := range strings.Split(string(body), "\n\n") {
		if !stanzaIsPackage(stanza, pkg) {
			continue
		}
		if m := aptVersionRe.FindStringSubmatch(stanza); m != nil {
			if v := stripPkgrel(m[1]); latest == "" || compareDotted(v, latest) > 0 {
				latest = v
			}
		}
	}
	if latest == "" {
		return RemoteState{Found: false}, nil
	}
	return RemoteState{Version: latest, Found: true}, nil
}

// stanzaIsPackage は Packages のスタンザが Package: <pkg> 行(完全一致)を持つか判定する。
// strings.Contains だと "crofty" が "crofty-extra" に誤マッチするため行単位で照合する。
func stanzaIsPackage(stanza, pkg string) bool {
	for _, line := range strings.Split(stanza, "\n") {
		if rest, ok := strings.CutPrefix(line, "Package:"); ok {
			return strings.TrimSpace(rest) == pkg
		}
	}
	return false
}

// RpmProbe は repomd.xml → primary.xml(圧縮ありうる)を辿り、package の版を読む。
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
		return RemoteState{}, fmt.Errorf("parse repodata/repomd.xml: %w", err)
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
	if raw, err = decompressPrimary(href, raw); err != nil {
		return RemoteState{}, err
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
		return RemoteState{}, fmt.Errorf("parse %s: %w", href, err)
	}
	// primary は過去版も載りうる(flat repo)。マッチするうち最も高い版を返す。
	latest := ""
	for _, p := range primary.Packages {
		if p.Name == pkg && (latest == "" || compareDotted(p.Version.Ver, latest) > 0) {
			latest = p.Version.Ver
		}
	}
	if latest == "" {
		return RemoteState{Found: false}, nil
	}
	return RemoteState{Version: latest, Found: true}, nil
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

// decompressPrimary は primary の href の拡張子から圧縮形式を見分けて展開する。
// createrepo_c は --general-compress-type で gz/xz/zstd(既定 zstd)を選べ、fury 等は非圧縮の
// primary.xml を配る。形式を推測せず拡張子で決め打ちし、知らない形式は生バイトを XML として
// 読ませずここで断る(そうしないと「XML が壊れている」という原因を指さない誤りになる)。
func decompressPrimary(href string, raw []byte) ([]byte, error) {
	var r io.Reader
	switch ext := path.Ext(href); ext {
	case ".xml":
		return raw, nil
	case ".gz":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", href, err)
		}
		defer zr.Close()
		r = zr
	case ".xz":
		xr, err := xz.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", href, err)
		}
		r = xr
	case ".zst", ".zstd":
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", href, err)
		}
		defer zr.Close()
		r = zr
	case ".bz2":
		r = bzip2.NewReader(bytes.NewReader(raw))
	default:
		return nil, fmt.Errorf("primary %s: unsupported compression %q (supported: .xml, .gz, .xz, .zst, .bz2)", href, ext)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", href, err)
	}
	return out, nil
}

// compareDotted はドット区切りの数値部を順に比べ -1/0/1 を返す(state.compareVersions と同方針)。
// 数値化できない部分は文字列比較にフォールバックする。flat repo の複数版から最新を選ぶのに使う。
func compareDotted(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		sa, sb := dottedPart(pa, i), dottedPart(pb, i)
		na, ea := strconv.Atoi(sa)
		nb, eb := strconv.Atoi(sb)
		if ea == nil && eb == nil {
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			continue
		}
		if sa != sb {
			return strings.Compare(sa, sb)
		}
	}
	return 0
}

func dottedPart(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return "0"
}
