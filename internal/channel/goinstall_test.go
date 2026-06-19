package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEscapeModule(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/widget":          "github.com/acme/widget",
		"github.com/ShiroDoromoto/wharfy": "github.com/!shiro!doromoto/wharfy",
		"github.com/Foo/Bar":              "github.com/!foo/!bar",
	}
	for in, want := range cases {
		if got := escapeModule(in); got != want {
			t.Errorf("escapeModule(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGoInstallInstallCommand(t *testing.T) {
	g := &GoInstall{Module: "github.com/o/r", InstallPath: "github.com/o/r/cmd/x", Version: "v1.2.0"}
	if got := g.InstallCommand(); got != "go install github.com/o/r/cmd/x@v1.2.0" {
		t.Errorf("InstallCommand = %q", got)
	}
	g.Version = ""
	if got := g.InstallCommand(); got != "go install github.com/o/r/cmd/x@latest" {
		t.Errorf("no-version InstallCommand = %q", got)
	}
}

func TestGoInstallPlanIsNoop(t *testing.T) {
	g := &GoInstall{Module: "github.com/o/r"}
	item, _ := g.Plan(context.Background())
	if item.Action != ActionNoop || item.OwnedArtifact != "" {
		t.Errorf("goinstall plan should be noop with no artifact: %+v", item)
	}
}

func TestGoInstallProbe(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/github.com/!shiro!doromoto/widget/@v/v0.1.0.info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0","Time":"2026-06-19T00:00:00Z"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	g := &GoInstall{Module: "github.com/ShiroDoromoto/widget", Version: "v0.1.0", Proxy: srv.URL, HTTP: srv.Client()}
	rs, err := g.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "v0.1.0" {
		t.Errorf("probe = %+v, want found v0.1.0 (path %q)", rs, gotPath)
	}

	// 別の版は proxy に無い → not found(エラーではない)。
	g.Version = "v9.9.9"
	rs, err = g.Probe(context.Background())
	if err != nil || rs.Found {
		t.Errorf("missing version should be not-found without error: rs=%+v err=%v", rs, err)
	}
}
