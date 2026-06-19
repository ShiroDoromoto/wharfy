package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubTapStoreExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/homebrew-x" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := &GitHubTapStore{Owner: "acme", Repo: "homebrew-x", API: srv.URL, HTTP: srv.Client()}
	if ok, err := s.Exists(context.Background()); err != nil || !ok {
		t.Errorf("existing repo: ok=%v err=%v", ok, err)
	}
	s.Repo = "missing"
	if ok, err := s.Exists(context.Background()); err != nil || ok {
		t.Errorf("missing repo should be false: ok=%v err=%v", ok, err)
	}
}

func TestGitHubTapStoreCreate(t *testing.T) {
	var createdPath, createdName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"acme"}`))
		case r.Method == "POST" && r.URL.Path == "/user/repos":
			createdPath = r.URL.Path
			b := make([]byte, 256)
			n, _ := r.Body.Read(b)
			createdName = string(b[:n])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	// owner == authed user → /user/repos に作る。
	s := &GitHubTapStore{Owner: "acme", Repo: "homebrew-x", Token: "tok", API: srv.URL, HTTP: srv.Client()}
	if err := s.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	if createdPath != "/user/repos" || !strings.Contains(createdName, `"homebrew-x"`) || !strings.Contains(createdName, `"auto_init":true`) {
		t.Errorf("create payload wrong: path=%q body=%q", createdPath, createdName)
	}
}

func TestEnsureRepo(t *testing.T) {
	// 既存 → 作らない。
	s := NewInMemoryTapStore()
	hb := &Homebrew{Project: "x", Tap: "acme/homebrew-x", Store: s}
	if created, _ := hb.EnsureRepo(context.Background()); created || s.Created != 0 {
		t.Errorf("existing repo should not create: created=%v count=%d", created, s.Created)
	}
	// 未作成 → 作る。
	s2 := NewInMemoryTapStore()
	s2.RepoExists = false
	hb2 := &Homebrew{Project: "x", Tap: "acme/homebrew-x", Store: s2}
	if created, _ := hb2.EnsureRepo(context.Background()); !created || s2.Created != 1 {
		t.Errorf("missing repo should be created once: created=%v count=%d", created, s2.Created)
	}
}
