package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/secret"
)

func hasProblem(res output.Result, code string) bool {
	for _, e := range res.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}

func TestResolveTokenEnvWins(t *testing.T) {
	keyring.MockInit()
	_ = secret.Set("package_repo_token", "from-keychain")
	t.Setenv("PACKAGE_REPO_TOKEN", "from-env")
	if got := resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token"); got != "from-env" {
		t.Errorf("env should win over keychain, got %q", got)
	}
}

func TestResolveTokenKeychainFallback(t *testing.T) {
	keyring.MockInit()
	t.Setenv("PACKAGE_REPO_TOKEN", "")
	_ = secret.Set("package_repo_token", "from-keychain")
	if got := resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token"); got != "from-keychain" {
		t.Errorf("should fall back to keychain, got %q", got)
	}
}

func TestResolveTokenNoneEmpty(t *testing.T) {
	keyring.MockInit()
	t.Setenv("PACKAGE_REPO_TOKEN", "")
	if got := resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token"); got != "" {
		t.Errorf("none set → empty, got %q", got)
	}
}

func TestRunAuthSavesToKeychain(t *testing.T) {
	keyring.MockInit()
	defer func(old func(string) (string, error)) { promptSecret = old }(promptSecret)
	promptSecret = func(string) (string, error) { return "tok-123", nil }

	res := runAuth(context.Background(), mustLookup(t, "auth"), []string{"fury"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if v, err := secret.Get("package_repo_token"); err != nil || v != "tok-123" {
		t.Errorf("token not stored under package_repo_token: (%q,%v)", v, err)
	}
	// 値が Result(message/data/next)に漏れていないこと。
	if blob := fmt.Sprintf("%+v", res); strings.Contains(blob, "tok-123") {
		t.Errorf("token value leaked into result: %s", blob)
	}
}

func TestRunAuthUnknownKind(t *testing.T) {
	res := runAuth(context.Background(), mustLookup(t, "auth"), []string{"nope"})
	if res.OK || !hasProblem(res, "config_invalid") {
		t.Errorf("unknown kind → ok=false config_invalid: %+v", res)
	}
}

func TestRunAuthEmptyToken(t *testing.T) {
	keyring.MockInit()
	defer func(old func(string) (string, error)) { promptSecret = old }(promptSecret)
	promptSecret = func(string) (string, error) { return "", nil }

	res := runAuth(context.Background(), mustLookup(t, "auth"), []string{"fury"})
	if res.OK || !hasProblem(res, "token_missing") {
		t.Errorf("empty token → ok=false token_missing: %+v", res)
	}
}

func TestRunAuthNoKindShowsHelp(t *testing.T) {
	res := runAuth(context.Background(), mustLookup(t, "auth"), nil)
	if !res.OK {
		t.Errorf("no kind → ok help, got: %+v", res)
	}
	if !strings.Contains(res.Message, "fury") {
		t.Errorf("help should list kinds: %q", res.Message)
	}
}
