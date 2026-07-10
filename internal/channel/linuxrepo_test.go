package channel

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func TestAptProbe(t *testing.T) {
	packages := "Package: widget\nVersion: 1.4.0\nArchitecture: amd64\n\nPackage: other\nVersion: 9.9.9\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/main/binary-amd64/Packages" {
			_, _ = w.Write([]byte(packages))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rs, err := (&AptProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "1.4.0" {
		t.Errorf("apt probe = %+v, want 1.4.0", rs)
	}

	// レイアウトが違って取得不可 → not found(エラーにしない)。
	rs, err = (&AptProbe{Repo: srv.URL + "/missing", HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil || rs.Found {
		t.Errorf("missing Packages → not found: rs=%+v err=%v", rs, err)
	}
}

// flat repo(Gemfury 等): <repo>/Packages 直下にメタデータ。過去版も全て載るので最新を返す。
// "widget" の prefix である "widget-extra" に誤マッチしないことも確認する。
func TestAptProbeFlatRepoLatest(t *testing.T) {
	packages := "Package: widget\nVersion: 0.11.0\nArchitecture: arm64\n\n" +
		"Package: widget-extra\nVersion: 9.9.9\nArchitecture: amd64\n\n" +
		"Package: widget\nVersion: 0.12.0\nArchitecture: amd64\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Packages" { // flat レイアウトのみ提供(dists は無い)
			_, _ = w.Write([]byte(packages))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rs, err := (&AptProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "0.12.0" {
		t.Errorf("flat apt probe = %+v, want latest 0.12.0 (not 0.11.0, not widget-extra)", rs)
	}
}

// rpm の primary に複数版が載るとき最新を返す。
func TestRpmProbeMultiVersionLatest(t *testing.T) {
	repomd := `<?xml version="1.0"?><repomd><data type="primary"><location href="repodata/primary.xml.gz"/></data></repomd>`
	primary := `<?xml version="1.0"?><metadata>` +
		`<package><name>widget</name><version ver="0.11.0"/></package>` +
		`<package><name>widget</name><version ver="0.12.0"/></package></metadata>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/repodata/primary.xml.gz":
			_, _ = w.Write(gz(primary))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rs, err := (&RpmProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "0.12.0" {
		t.Errorf("rpm probe = %+v, want latest 0.12.0", rs)
	}
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

func xzip(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := xz.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zst(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// rpmProbeXML は下の圧縮テストが共通で使う primary の中身。
const rpmProbeXML = `<?xml version="1.0"?><metadata><package><name>widget</name><version ver="2.0.1"/></package></metadata>`

// rpmProbeBz2 は rpmProbeXML を bzip2 -9 で固めたもの。標準ライブラリに bzip2 の encoder が
// 無いので、生成せず固定バイト列を置く。
const rpmProbeBz2 = "QlpoOTFBWSZTWTVmCMMAAAqZgFAB8Aeur93AIABIaFBsUaNDT1NAEqZGoAANDNSvEnCoyScmJBl2YJ7b6UpchhXYWBiA4qFAiDwpBVJDVqUGY30dyMchV4h0JaIPJJz/F3JFOFCQNWYIww=="

// createrepo_c は --general-compress-type で primary の圧縮を選べる(既定 zstd)。fury は非圧縮の
// primary.xml を配る。どれを指されても版を読めること。
func TestRpmProbeCompressions(t *testing.T) {
	bz2, err := base64.StdEncoding.DecodeString(rpmProbeBz2)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		href string
		body []byte
	}{
		{"plain", "repodata/primary.xml", []byte(rpmProbeXML)},
		{"gzip", "repodata/primary.xml.gz", gz(rpmProbeXML)},
		{"xz", "repodata/primary.xml.xz", xzip(t, rpmProbeXML)},
		{"zstd", "repodata/primary.xml.zst", zst(t, rpmProbeXML)},
		{"bzip2", "repodata/primary.xml.bz2", bz2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(rpmRepo(tc.href, tc.body))
			defer srv.Close()

			rs, err := (&RpmProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
			if err != nil {
				t.Fatal(err)
			}
			if !rs.Found || rs.Version != "2.0.1" {
				t.Errorf("rpm probe = %+v, want 2.0.1", rs)
			}
		})
	}
}

// 知らない圧縮形式は、生バイトを XML として読ませず「読めない形式」だと言って断る。
// 展開せずに渡すと "XML syntax error ... invalid UTF-8" という原因を指さない誤りになる。
func TestRpmProbeUnsupportedCompression(t *testing.T) {
	srv := httptest.NewServer(rpmRepo("repodata/primary.xml.lz4", []byte("\x04\x22\x4d\x18 not xml")))
	defer srv.Close()

	_, err := (&RpmProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err == nil {
		t.Fatal("probe on primary.xml.lz4 = nil error, want unsupported compression")
	}
	if !strings.Contains(err.Error(), `unsupported compression ".lz4"`) {
		t.Errorf("probe error = %v, want it to name the compression it cannot read", err)
	}
}

// rpmRepo は primary を href に置いた最小の yum repo を返す。
func rpmRepo(href string, primary []byte) http.HandlerFunc {
	repomd := `<?xml version="1.0"?><repomd><data type="primary"><location href="` + href + `"/></data></repomd>`
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/" + href:
			_, _ = w.Write(primary)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}
