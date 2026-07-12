package main

import (
	"context"
	"strings"
	"testing"
)

// secrets の芯: 構成(channels:)から要る env が一意に決まり、CI での出どころまで言えること。

// 自リポにしか書かないチャネルだけなら、GITHUB_TOKEN は Actions が配る分で足りる(登録不要)。
func TestSecretsBuiltinTokenWhenNoCrossRepoChannel(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases, container]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	res := runSecrets(context.Background(), mustLookup(t, "secrets"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	d := res.Data.(secretsData)
	if len(d.Credentials) != 1 || d.Credentials[0].Env != "GITHUB_TOKEN" {
		t.Fatalf("only GITHUB_TOKEN should be needed: %+v", d.Credentials)
	}
	got := d.Credentials[0]
	if got.Source != sourceBuiltin || got.Register != "" || !got.Met {
		t.Errorf("builtin token needs no registration: %+v", got)
	}
	if d.Actions.Env["GITHUB_TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" {
		t.Errorf("should pass the builtin token: %+v", d.Actions.Env)
	}
	if !hasPermission(d.Actions.Permissions, "packages: write") || !hasPermission(d.Actions.Permissions, "contents: write") {
		t.Errorf("container needs contents+packages write: %+v", d.Actions.Permissions)
	}
}

// tap へ書くチャネルが在れば、組み込みトークンでは開かない——PAT の登録を求める。
func TestSecretsDemandsPATForCrossRepoChannel(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases, homebrew]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")

	res := runSecrets(context.Background(), mustLookup(t, "secrets"), nil)
	d := res.Data.(secretsData)
	got := d.Credentials[0]
	if got.Env != "GITHUB_TOKEN" || got.Source != sourceSecret {
		t.Fatalf("tap writes need a registered PAT: %+v", got)
	}
	if got.Met {
		t.Error("unset token must not report met")
	}
	if got.Register != "gh secret set WHARFY_GITHUB_TOKEN" {
		t.Errorf("should hand over the gh command: %q", got.Register)
	}
	if d.Actions.Env["GITHUB_TOKEN"] != "${{ secrets.WHARFY_GITHUB_TOKEN }}" {
		t.Errorf("workflow should read the registered secret: %+v", d.Actions.Env)
	}
	if len(d.Actions.Notes) == 0 || !strings.Contains(d.Actions.Notes[0], "homebrew writes to your homebrew tap repo") {
		t.Errorf("should say why the builtin token is not enough: %+v", d.Actions.Notes)
	}
	if !hasNextDo(res, "gh secret set WHARFY_GITHUB_TOKEN") {
		t.Errorf("next should register the secret: %+v", res.Next)
	}
}

// apt/rpm と aur は自前の秘密を要る。署名は宣言したときだけ。
func TestSecretsCollectsPerChannelCredentials(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt, aur]\nsign:\n  identity: \"Developer ID Application: Acme\"\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PACKAGE_REPO_TOKEN", "")
	t.Setenv("AUR_SSH_KEY", "")

	res := runSecrets(context.Background(), mustLookup(t, "secrets"), nil)
	d := res.Data.(secretsData)
	byEnv := map[string]credentialNeed{}
	for _, n := range d.Credentials {
		byEnv[n.Env] = n
	}
	for _, env := range []string{"GITHUB_TOKEN", "PACKAGE_REPO_TOKEN", "AUR_SSH_KEY", "WHARFY_SIGN_P12", "WHARFY_SIGN_P12_PASSWORD"} {
		if _, ok := byEnv[env]; !ok {
			t.Errorf("%s should be listed: %+v", env, d.Credentials)
		}
	}
	if chans := byEnv["PACKAGE_REPO_TOKEN"].Channels; len(chans) != 1 || chans[0] != "apt" {
		t.Errorf("PACKAGE_REPO_TOKEN belongs to apt: %+v", chans)
	}
	if !strings.Contains(strings.Join(d.Actions.Notes, " "), "macos runner") {
		t.Errorf("signing should warn about the runner: %+v", d.Actions.Notes)
	}
}

// 署名を宣言していなければ、署名の秘密は求めない(持ち込み署名を尊重する既定)。
func TestSecretsOmitsSigningWhenNotDeclared(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("WHARFY_SIGN_IDENTITY", "")
	t.Setenv("WHARFY_SIGN_P12", "")

	res := runSecrets(context.Background(), mustLookup(t, "secrets"), nil)
	for _, n := range res.Data.(secretsData).Credentials {
		if strings.HasPrefix(n.Env, "WHARFY_SIGN") {
			t.Errorf("no sign: declared → no signing secrets: %+v", n)
		}
	}
}

// envelope が契約(schemas/secrets.json)に valid であること。
func TestSecretsJSONValidatesSchema(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [releases, homebrew, apt, container]\n")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	res := runSecrets(context.Background(), mustLookup(t, "secrets"), nil)
	validateAgainst(t, "https://wharfy.io/schemas/v1/secrets.json", res)
}

func hasPermission(perms []string, want string) bool {
	for _, p := range perms {
		if p == want {
			return true
		}
	}
	return false
}
