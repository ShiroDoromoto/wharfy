package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/state"
)

func swapOCIProbeBase(url string) func() {
	prev := ociProbeBase
	ociProbeBase = url
	return func() { ociProbeBase = prev }
}

// container: 記録版のタグが ghcr に無い → drift(missing)。
func TestStatusContainerMissing(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"container": {Version: "1.0.0", Target: "ghcr.io/acme/demo", At: "t"}}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // タグが無い
	}))
	defer srv.Close()
	defer swapOCIProbeBase(srv.URL)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := findChannel(out.Channels, "container")
	if c == nil || c.Source != state.SourceDrift || c.Drift == nil || c.Drift.Kind != state.DriftMissing {
		t.Fatalf("container should be drift missing: %+v", c)
	}
}

// container: タグが在る → probed(一致)。
func TestStatusContainerProbed(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [container]\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"container": {Version: "1.0.0", Target: "ghcr.io/acme/demo", At: "t"}}
	_ = state.Save(root, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		w.WriteHeader(http.StatusOK) // タグ在り
	}))
	defer srv.Close()
	defer swapOCIProbeBase(srv.URL)()

	out, _ := buildStatus(context.Background(), true)
	c := findChannel(out.Channels, "container")
	if c == nil || c.Source != state.SourceProbed || c.Drift != nil {
		t.Fatalf("container should be probed/in-sync: %+v", c)
	}
}

// apt: hosted repo の Packages 版が記録より古い → drift(behind)。repo URL は httptest。
func TestStatusAptDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/main/binary-amd64/Packages" {
			_, _ = w.Write([]byte("Package: demo\nVersion: 1.1.0\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\napt:\n  repo: "+srv.URL+"\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"apt": {Version: "1.2.0", Target: srv.URL, At: "t"}}
	_ = state.Save(root, st)

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := findChannel(out.Channels, "apt")
	if c == nil || c.Source != state.SourceDrift || c.Drift == nil || c.Drift.Kind != state.DriftBehind {
		t.Fatalf("apt should be drift behind: %+v", c)
	}
}

// rpm: repomd→primary の版が記録と一致 → probed。
func TestStatusRpmProbed(t *testing.T) {
	repomd := `<?xml version="1.0"?><repomd><data type="primary"><location href="repodata/primary.xml.gz"/></data></repomd>`
	primary := `<?xml version="1.0"?><metadata><package><name>demo</name><version ver="1.2.0"/></package></metadata>`
	var pg bytes.Buffer
	gw := gzip.NewWriter(&pg)
	_, _ = gw.Write([]byte(primary))
	_ = gw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/repodata/primary.xml.gz":
			_, _ = w.Write(pg.Bytes())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [rpm]\nrpm:\n  repo: "+srv.URL+"\n")
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"rpm": {Version: "1.2.0", Target: srv.URL, At: "t"}}
	_ = state.Save(root, st)

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := findChannel(out.Channels, "rpm")
	if c == nil || c.Source != state.SourceProbed || c.Drift != nil {
		t.Fatalf("rpm should be probed/in-sync: %+v", c)
	}
}
