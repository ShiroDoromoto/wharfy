package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
)

// swapGoinstallProxy は module proxy を httptest に差し替える。
func swapGoinstallProxy(url string) func() {
	prev := goinstallProxy
	goinstallProxy = url
	return func() { goinstallProxy = prev }
}

// found: tag があり proxy に版がある → go install コマンドを案内。
func TestPublishGoinstallFound(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Version":"v1.0.0"}`))
	}))
	defer srv.Close()
	defer swapGoinstallProxy(srv.URL)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"goinstall"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || len(pd.Plan) != 1 || pd.Plan[0].Action != channel.ActionNoop {
		t.Fatalf("goinstall should be noop, not applied: %+v", pd)
	}
	if !hasNextDo(res, "go install github.com/acme/demo/cmd/demo@v1.0.0") {
		t.Errorf("should advise the go install command: %+v", res.Next)
	}
	validateAgainst(t, publishSchemaID, res)
}

// no tag → 版未確定。tag を促し、install は @latest で案内。
func TestPublishGoinstallNoTag(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"goinstall"})
	if !res.OK {
		t.Fatalf("no tag is not a failure for goinstall: %+v", res)
	}
	if !hasNextDo(res, "git tag vX.Y.Z && git push --tags") {
		t.Errorf("should prompt to tag: %+v", res.Next)
	}
	if !hasNextDo(res, "go install github.com/acme/demo/cmd/demo@latest") {
		t.Errorf("should advise @latest when untagged: %+v", res.Next)
	}
}

// proxy にまだ無い → エラーではなく案内(public/push を促す)。
func TestPublishGoinstallNotOnProxy(t *testing.T) {
	root := scratchModule(t)
	tagScratch(t, root, "v1.0.0")
	chdir(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	defer swapGoinstallProxy(srv.URL)()

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"goinstall"})
	if !res.OK {
		t.Fatalf("not-on-proxy is advisory, not a failure: %+v", res)
	}
	if !hasNextDo(res, "git push --tags") {
		t.Errorf("should advise pushing the tag: %+v", res.Next)
	}
}
