package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubWingetSubmit は fork→branch→commit→PR の一連を mock GitHub API で検証する
// (実 microsoft/winget-pkgs には触れない)。
func TestGitHubWingetSubmit(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "GET" && r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"tester"}`))
		case r.Method == "POST" && r.URL.Path == "/repos/microsoft/winget-pkgs/forks":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && r.URL.Path == "/repos/tester/winget-pkgs/git/ref/heads/master":
			_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
		case r.Method == "POST" && r.URL.Path == "/repos/tester/winget-pkgs/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound) // 新規ファイル
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/repos/microsoft/winget-pkgs/pulls":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/microsoft/winget-pkgs/pull/123"}`))
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	s := &GitHubWingetSubmitter{Token: "tok", Central: "microsoft/winget-pkgs", BaseBranch: "master", API: srv.URL, HTTP: srv.Client()}
	in := wingetInput()
	files := GenerateWingetManifests(in)

	url, err := s.Submit(context.Background(), in, files)
	if err != nil {
		t.Fatalf("submit: %v\ncalls: %v", err, calls)
	}
	if url != "https://github.com/microsoft/winget-pkgs/pull/123" {
		t.Errorf("PR url = %q", url)
	}
	// fork → branch → 3 ファイル PUT → PR の順で叩く。
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"POST /repos/microsoft/winget-pkgs/forks",
		"POST /repos/tester/winget-pkgs/git/refs",
		"PUT /repos/tester/winget-pkgs/contents/manifests/s/ShiroDoromoto/widget/1.2.3/ShiroDoromoto.widget.yaml",
		"POST /repos/microsoft/winget-pkgs/pulls",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
	puts := strings.Count(joined, "PUT /repos/tester/winget-pkgs/contents/")
	if puts != 3 {
		t.Errorf("expected 3 manifest PUTs, got %d", puts)
	}
}

// TestGitHubWingetSubmitNeedsToken: トークン無しは即エラー(fork すらしない)。
func TestGitHubWingetSubmitNeedsToken(t *testing.T) {
	s := &GitHubWingetSubmitter{}
	if _, err := s.Submit(context.Background(), wingetInput(), nil); err == nil {
		t.Error("expected error without token")
	}
}
