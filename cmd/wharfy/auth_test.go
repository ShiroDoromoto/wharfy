package main

import (
	"context"
	"fmt"
	"io"
	"os"
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

// --print は保存済みの資格情報を、そのまま使える形で stdout に出す(パイプで CI の secret に渡せる)。
// keychain の中身は go-keyring の包み(go-keyring-base64:…)で入っていることがあり、利用者が
// 自力で取り出すと無言の 401 を踏む。wharfy が読むのと同じ経路で渡すのが要点。
func TestAuthPrintWritesTheUsableValueToStdout(t *testing.T) {
	keyring.MockInit()
	if err := secret.Set("package_repo_token", "tok-123"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		res := runAuthPrint(mustLookup(t, "auth"), []string{"fury"})
		if !res.OK {
			t.Fatalf("a stored credential should print: %+v", res)
		}
	})
	if strings.TrimSpace(out) != "tok-123" {
		t.Errorf("stdout must carry the value and nothing else, got %q", out)
	}
}

// 保存されていなければ、印字ではなく「先に預けろ」と言う。
func TestAuthPrintWithoutStoredCredential(t *testing.T) {
	keyring.MockInit()
	res := runAuthPrint(mustLookup(t, "auth"), []string{"fury"})
	if res.OK || !hasProblem(res, output.ErrTokenMissing) {
		t.Fatalf("nothing to print should fail with token_missing: %+v", res)
	}
}

// --json とは併用しない —— 機械可読の出力に載せた秘密は、それを読む agent の文脈に残る。
func TestAuthPrintRefusesJSON(t *testing.T) {
	keyring.MockInit()
	_ = secret.Set("package_repo_token", "tok-123")
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })

	res := runAuthPrint(mustLookup(t, "auth"), []string{"fury"})
	if res.OK || !hasProblem(res, output.ErrConfigInvalid) {
		t.Fatalf("a secret must never go into the json envelope: %+v", res)
	}
}

// keychain に預けてあるなら、secrets の登録コマンドはそこから渡す形で出す(包み方を知らずに済む)。
func TestSecretsRegisterUsesAuthPrintWhenStored(t *testing.T) {
	keyring.MockInit()
	_ = secret.Set("package_repo_token", "tok-123")
	if got := registerCommand("PACKAGE_REPO_TOKEN"); got != "wharfy auth fury --print | gh secret set PACKAGE_REPO_TOKEN" {
		t.Errorf("should hand over the pipe that avoids the wrapping: %q", got)
	}
	keyring.MockInit() // 預けていない状態に戻す
	if got := registerCommand("PACKAGE_REPO_TOKEN"); got != "gh secret set PACKAGE_REPO_TOKEN" {
		t.Errorf("with nothing stored, the plain gh command: %q", got)
	}
}

// captureStdout は fn の間の stdout を集める。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return string(b)
}
