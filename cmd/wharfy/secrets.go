package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// secrets.go — 「CI で回すのに何を登録すればいいか」を wharfy 自身が語る(D-12)。
//
// 資格情報は wharfy.yaml の channels: から一意に決まるのに、これまで語り口が無く、利用者は
// README とコードを突き合わせて gh secret set を組み立てていた。requirement のヒントも
// `export FOO=…`(手元のシェル)しか知らなかった。ここは同じ 1 枚の表を、手元と GitHub Actions の
// 両方の言葉で見せる。単一真実は registry(ChannelCredentials / CrossRepoChannels / ActionsPermissions)。

// secretsData は `wharfy secrets --json`(schemas/secrets.json)。
type secretsData struct {
	Credentials []credentialNeed `json:"credentials"`
	Actions     actionsGuide     `json:"actions"`
}

// credentialNeed は 1 つの env と、それを要るチャネル・充足状況・CI での出どころ。
type credentialNeed struct {
	Env      string   `json:"env"`
	Purpose  string   `json:"purpose"`
	Channels []string `json:"channels"`
	Met      bool     `json:"met"` // いま手元の環境で解決できるか
	// Source は CI での出どころ。actions_builtin(Actions が配る)/ repo_secret(登録が要る)。
	Source string `json:"source"`
	// Register は登録用のコマンド(repo_secret のときだけ)。
	Register string `json:"register,omitempty"`
}

const (
	sourceBuiltin = "actions_builtin"
	sourceSecret  = "repo_secret"
)

// actionsGuide は workflow に貼る断片。permissions と env、それに前提の注記。
type actionsGuide struct {
	Permissions []string          `json:"permissions,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
}

// runSecrets は解決済み構成から、要る資格情報と CI での渡し方を組み立てる。
func runSecrets(_ context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, loadErr := config.Load(root)
	if loadErr != nil {
		res := output.New(c.Name, "wharfy.yaml is invalid — cannot tell which credentials you need", false)
		res.Errors = []output.Problem{{Code: output.ErrConfigInvalid, Message: loadErr.Error(), Hint: configInvalidHint}}
		res.Next = []output.NextDo{{Reason: "fix the file then re-run", Do: "wharfy secrets"}}
		return res
	}
	cfg, _ := config.NewResolver(root).Resolve(in)

	data := buildSecrets(cfg, in)
	if !flagJSON {
		// 表と貼れる断片は stderr へ(init と同じ作法。stdout は envelope に譲る)。
		printSecretsHuman(os.Stderr, data)
	}
	res := output.New(c.Name, secretsMessage(data), true)
	res.Data = data
	res.Next = secretsNext(data)
	return res
}

// buildSecrets は channels:(＋ sign:)から要る env を引き当て、CI での出どころまで決める。
func buildSecrets(cfg config.Config, in config.File) secretsData {
	// env → それを要るチャネル。宣言順ではなくチャネル順で読めるよう、後で名前順に均す。
	byEnv := map[string][]string{}
	perms := map[string]bool{}
	crossRepo := map[string]string{}

	for _, ch := range cfg.Channels {
		name := ch.Name
		for _, env := range registry.ChannelCredentials[name] {
			byEnv[env] = append(byEnv[env], name)
		}
		for _, p := range registry.ActionsPermissions[name] {
			perms[p] = true
		}
		if where, ok := registry.CrossRepoChannels[name]; ok {
			crossRepo[name] = where
		}
	}
	// 署名は宣言したときだけ要る(持ち込み署名を尊重する既定は no-op)。
	if signDeclared(in) {
		for _, env := range registry.SignCredentials {
			byEnv[env] = append(byEnv[env], "sign")
		}
	}

	needs := make([]credentialNeed, 0, len(byEnv))
	for env, chans := range byEnv {
		spec := registry.Credentials[env]
		sort.Strings(chans)
		n := credentialNeed{
			Env:      env,
			Purpose:  spec.Purpose,
			Channels: chans,
			Met:      credentialMet(env),
			Source:   sourceSecret,
		}
		// GITHUB_TOKEN は Actions が自前で配る——ただし自リポにしか書けない。tap/bucket/fork へ
		// 書くチャネルが 1 つでも在れば、PAT を登録して渡すしかない。
		if env == "GITHUB_TOKEN" && len(crossRepo) == 0 {
			n.Source = sourceBuiltin
		}
		if n.Source == sourceSecret {
			n.Register = "gh secret set " + secretName(env)
		}
		needs = append(needs, n)
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].Env < needs[j].Env })

	return secretsData{Credentials: needs, Actions: actionsFor(needs, perms, crossRepo)}
}

// actionsFor は workflow に貼る permissions / env と、その理由の注記を組む。
func actionsFor(needs []credentialNeed, perms map[string]bool, crossRepo map[string]string) actionsGuide {
	g := actionsGuide{Env: map[string]string{}}
	for p := range perms {
		g.Permissions = append(g.Permissions, p)
	}
	sort.Strings(g.Permissions)

	for _, n := range needs {
		if n.Source == sourceBuiltin {
			g.Env[n.Env] = "${{ secrets.GITHUB_TOKEN }}"
			continue
		}
		g.Env[n.Env] = "${{ secrets." + secretName(n.Env) + " }}"
	}
	if len(crossRepo) > 0 {
		g.Notes = append(g.Notes, "GITHUB_TOKEN must be a PAT (repo scope): "+crossRepoLine(crossRepo)+
			" — the token Actions hands the workflow can only write to this repository")
	}
	if _, ok := g.Env["WHARFY_SIGN_P12"]; ok {
		g.Notes = append(g.Notes, "codesigning needs a macos runner (codesign is macOS-only); store the .p12 base64-encoded")
	}
	return g
}

// crossRepoLine は「どのチャネルがどこへ書くか」を 1 行に畳む(名前順)。
func crossRepoLine(crossRepo map[string]string) string {
	names := make([]string, 0, len(crossRepo))
	for n := range crossRepo {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+" writes to "+crossRepo[n])
	}
	return strings.Join(parts, ", ")
}

// secretName は env 名から登録先シークレット名を決める。GITHUB_TOKEN だけは同名で登録できない
// (Actions の予約名)ので別名にする。
func secretName(env string) string {
	if env == "GITHUB_TOKEN" {
		return "WHARFY_GITHUB_TOKEN"
	}
	return env
}

// credentialMet はいま手元でその env が解決できるか(fury だけ keychain の補助が効く)。
func credentialMet(env string) bool {
	if env == "PACKAGE_REPO_TOKEN" {
		return resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token") != ""
	}
	return os.Getenv(env) != ""
}

// signDeclared は sign を宣言しているか(yaml か env のどちらかで identity/p12 が在る)。
func signDeclared(in config.File) bool {
	if in.Sign != nil && (in.Sign.Identity != "" || in.Sign.P12 != "") {
		return true
	}
	return os.Getenv(envSignIdentity) != "" || os.Getenv(envSignP12) != ""
}

func secretsMessage(d secretsData) string {
	var missing int
	for _, n := range d.Credentials {
		if !n.Met {
			missing++
		}
	}
	if len(d.Credentials) == 0 {
		return "no credentials needed for the configured channels"
	}
	if missing == 0 {
		return fmt.Sprintf("%d credential(s) needed; all resolvable here", len(d.Credentials))
	}
	return fmt.Sprintf("%d credential(s) needed; %d not set in this environment", len(d.Credentials), missing)
}

// secretsNext は未登録のシークレットを登録する手(gh)を先に出し、次いで実際の配布へ繋ぐ。
func secretsNext(d secretsData) []output.NextDo {
	var next []output.NextDo
	for _, n := range d.Credentials {
		if n.Register == "" {
			continue
		}
		next = append(next, output.NextDo{Reason: "register for CI: " + n.Env + " (" + n.Purpose + ")", Do: n.Register})
	}
	next = append(next, output.NextDo{Reason: "see what would be published", Do: "wharfy publish --dry-run"})
	return next
}

// printSecretsHuman は表と、workflow へそのまま貼れる断片を出す。
func printSecretsHuman(w io.Writer, d secretsData) {
	if len(d.Credentials) == 0 {
		return
	}
	fmt.Fprintln(w, "credentials your channels need (wharfy reads them from the environment):")
	for _, n := range d.Credentials {
		mark := "✗"
		if n.Met {
			mark = "✓"
		}
		fmt.Fprintf(w, "  %s %-24s %s\n", mark, n.Env, strings.Join(n.Channels, ", "))
		fmt.Fprintf(w, "    %s\n", n.Purpose)
	}

	fmt.Fprintln(w, "\nin a GitHub Actions workflow:")
	if len(d.Actions.Permissions) > 0 {
		fmt.Fprintln(w, "  permissions:")
		for _, p := range d.Actions.Permissions {
			fmt.Fprintln(w, "    "+p)
		}
	}
	if len(d.Actions.Env) > 0 {
		fmt.Fprintln(w, "  env:")
		for _, env := range sortedKeys(d.Actions.Env) {
			fmt.Fprintf(w, "    %s: %s\n", env, d.Actions.Env[env])
		}
	}
	for _, note := range d.Actions.Notes {
		fmt.Fprintln(w, "  note: "+note)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
