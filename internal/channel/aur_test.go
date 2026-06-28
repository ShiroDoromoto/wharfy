package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func aurInput() AurInput {
	return AurInput{
		Package: "widget-bin", Project: "widget", Version: "1.2.3",
		License: "MIT", Description: "a widget", Homepage: "https://github.com/acme/widget-demo",
		Maintainer: "acme <a@b>",
		Sources: []AurSource{
			{Arch: "amd64", URL: "https://x/widget_1.2.3_linux_amd64.tar.gz", SHA256: "aa"},
			{Arch: "arm64", URL: "https://x/widget_1.2.3_linux_arm64.tar.gz", SHA256: "bb"},
		},
	}
}

func TestGeneratePKGBUILD(t *testing.T) {
	p := GeneratePKGBUILD(aurInput())
	for _, want := range []string{
		"pkgname=widget-bin",
		"pkgver=1.2.3",
		"arch=('aarch64' 'x86_64')", // amd64→x86_64, arm64→aarch64, sorted (aarch64 < x86_64)
		"provides=('widget')",
		"conflicts=('widget')",
		"source_x86_64=(\"widget-bin-1.2.3-x86_64.tar.gz::https://x/widget_1.2.3_linux_amd64.tar.gz\")",
		"sha256sums_x86_64=('aa')",
		`install -Dm755 "widget" "$pkgdir/usr/bin/widget"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("PKGBUILD missing %q\n---\n%s", want, p)
		}
	}
}

func TestGenerateSRCINFO(t *testing.T) {
	s := GenerateSRCINFO(aurInput())
	for _, want := range []string{"pkgbase = widget-bin", "\tpkgver = 1.2.3", "\tarch = x86_64", "\tarch = aarch64", "pkgname = widget-bin"} {
		if !strings.Contains(s, want) {
			t.Errorf(".SRCINFO missing %q\n---\n%s", want, s)
		}
	}
}

func TestAurDependsEmitted(t *testing.T) {
	in := aurInput()
	in.Depends = []string{"ffmpeg>=6.0", "git"}
	in.OptDepends = []string{"fzf"}
	p := GeneratePKGBUILD(in)
	if want := "depends=('ffmpeg>=6.0' 'git')"; !strings.Contains(p, want) {
		t.Errorf("PKGBUILD missing %q\n---\n%s", want, p)
	}
	if want := "optdepends=('fzf')"; !strings.Contains(p, want) {
		t.Errorf("PKGBUILD missing %q\n---\n%s", want, p)
	}
	s := GenerateSRCINFO(in)
	for _, want := range []string{"\tdepends = ffmpeg>=6.0", "\tdepends = git", "\toptdepends = fzf"} {
		if !strings.Contains(s, want) {
			t.Errorf(".SRCINFO missing %q\n---\n%s", want, s)
		}
	}
}

func TestAurDependsOmittedWhenEmpty(t *testing.T) {
	p := GeneratePKGBUILD(aurInput())
	if strings.Contains(p, "depends=") {
		t.Errorf("no deps → no depends/optdepends line\n%s", p)
	}
}

func TestAurFiles(t *testing.T) {
	f := aurInput().Files()
	if _, ok := f["PKGBUILD"]; !ok {
		t.Error("missing PKGBUILD")
	}
	if _, ok := f[".SRCINFO"]; !ok {
		t.Error("missing .SRCINFO")
	}
}

func TestAurProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rpc/v5/info/widget-bin") {
			_, _ = w.Write([]byte(`{"results":[{"Version":"1.2.3-1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	p := &AurProbe{Base: srv.URL, HTTP: srv.Client()}
	rs, err := p.Probe(context.Background(), "widget-bin")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "1.2.3" { // pkgrel(-1)を落とす
		t.Errorf("probe = %+v, want found 1.2.3", rs)
	}

	rs, _ = p.Probe(context.Background(), "missing-bin")
	if rs.Found {
		t.Error("missing package should be not-found")
	}
}
