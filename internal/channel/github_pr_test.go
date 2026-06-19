package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// submit が fork→base ref→branch→file commit→PR を順に叩き、PR URL を返すことを httptest で検証。
func TestGhPRSubmit(t *testing.T) {
	var putPath string
	var opened bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case r.Method == "POST" && r.URL.Path == "/repos/Up/stream/forks":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && r.URL.Path == "/repos/me/stream/git/ref/heads/master":
			_, _ = w.Write([]byte(`{"object":{"sha":"base123"}}`))
		case r.Method == "POST" && r.URL.Path == "/repos/me/stream/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/me/stream/contents/"):
			w.WriteHeader(http.StatusNotFound) // 新規ファイル
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/repos/me/stream/contents/"):
			putPath = strings.TrimPrefix(r.URL.Path, "/repos/me/stream/contents/")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/repos/Up/stream/pulls":
			opened = true
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/Up/stream/pull/7"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	g := &ghPR{Token: "tok", API: srv.URL, HTTP: srv.Client()}
	url, err := g.submit(context.Background(), "Up/stream", "master", "wharfy-x-1.0.0",
		map[string]string{"Formula/x/x.rb": "class X < Formula\nend\n"}, "msg", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/Up/stream/pull/7" {
		t.Errorf("PR url = %q", url)
	}
	if putPath != "Formula/x/x.rb" || !opened {
		t.Errorf("expected file PUT to Formula/x/x.rb and PR opened: put=%q opened=%v", putPath, opened)
	}
}

func TestCoreFormulaPath(t *testing.T) {
	if got := CoreFormulaPath("Wharfy"); got != "Formula/w/wharfy.rb" {
		t.Errorf("CoreFormulaPath = %q, want Formula/w/wharfy.rb", got)
	}
}
