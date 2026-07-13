package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOCIProbe(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"token":"anon-token"}`))
		case r.URL.Path == "/v2/acme/widget/manifests/1.2.3":
			if r.Header.Get("Authorization") == "Bearer anon-token" {
				sawAuth = true
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := &OCIProbe{Image: "ghcr.io/acme/widget", Base: srv.URL, HTTP: srv.Client()}
	rs, err := p.Probe(context.Background(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "1.2.3" || !sawAuth {
		t.Errorf("probe = %+v, sawAuth=%v", rs, sawAuth)
	}

	rs, err = p.Probe(context.Background(), "9.9.9")
	if err != nil || rs.Found {
		t.Errorf("absent tag → not found: rs=%+v err=%v", rs, err)
	}
}

// Digest は manifest の digest を返す。tag は動かせるので、2 つの tag(:<version> と :latest)が
// 同じ image を指しているかは、これでしか言えない(verify の container が使う)。
func TestOCIDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"token":"anon-token"}`))
		case "/v2/acme/widget/manifests/1.2.3", "/v2/acme/widget/manifests/latest":
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := &OCIProbe{Image: "ghcr.io/acme/widget", Base: srv.URL, HTTP: srv.Client()}
	version, found, err := p.Digest(context.Background(), "1.2.3")
	if err != nil || !found || version != "sha256:abc" {
		t.Fatalf("digest = %q found=%v err=%v", version, found, err)
	}
	latest, found, err := p.Digest(context.Background(), "latest")
	if err != nil || !found || latest != version {
		t.Errorf("latest should resolve to the same image: %q found=%v err=%v", latest, found, err)
	}
	if _, found, err := p.Digest(context.Background(), "9.9.9"); err != nil || found {
		t.Errorf("absent tag → not found: found=%v err=%v", found, err)
	}
}

func TestJSONField(t *testing.T) {
	if v := jsonField(`{"a":1,"token":"xyz","b":2}`, "token"); v != "xyz" {
		t.Errorf("jsonField = %q", v)
	}
	if v := jsonField(`{"access_token":"qq"}`, "token"); v != "" {
		t.Errorf("missing key should be empty, got %q", v)
	}
}

func TestRegistrySplit(t *testing.T) {
	if registryHost("ghcr.io/acme/widget") != "ghcr.io" || registryRepo("ghcr.io/acme/widget") != "acme/widget" {
		t.Error("registry host/repo split wrong")
	}
	if !strings.Contains((&OCIProbe{Image: "ghcr.io/x/y"}).base(), "https://ghcr.io") {
		t.Error("default base should use image host")
	}
}
