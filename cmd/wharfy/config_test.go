package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

const configSchemaID = "https://wharfy.io/schemas/v1/config.json"

// stubResolver は git / go list に触れず固定値で解決する(テストを決定的に保つ)。
func stubResolver(mains []string) *config.Resolver {
	return &config.Resolver{
		Root:       "/fake",
		OriginURL:  func(string) (string, error) { return "https://github.com/acme/mytool.git", nil },
		MainPkgs:   func(string) ([]string, error) { return mains, nil },
		ModulePath: func(string) (string, error) { return "github.com/acme/mytool", nil },
	}
}

// TestConfigJSONValidatesSchema: 解決成功時の config 出力が schemas/config.json に valid。
func TestConfigJSONValidatesSchema(t *testing.T) {
	cfg, err := stubResolver([]string{"./cmd/mytool"}).Resolve(config.File{})
	if err != nil {
		t.Fatal(err)
	}
	res := output.New("config", "resolved config for "+cfg.Project, true)
	res.Data = cfg
	res.Next = []output.NextDo{{Reason: "build with this config", Do: "wharfy build"}}
	validateAgainst(t, configSchemaID, res)
}

// TestConfigAmbiguousValidatesSchema: main 曖昧で停止した時の出力も契約に valid
// (config.json は data 必須。部分解決した実効設定を載せる)。
func TestConfigAmbiguousValidatesSchema(t *testing.T) {
	cfg, rerr := stubResolver([]string{"./cmd/a", "./cmd/b"}).Resolve(config.File{})
	if rerr == nil {
		t.Fatal("expected ambiguous main error")
	}
	res := output.New("config", "cannot resolve 'main' (ambiguous)", false)
	res.Data = cfg
	res.Errors = []output.Problem{{
		Code:    output.ErrMainAmbiguous,
		Message: rerr.Error(),
		Hint:    "set 'main' in wharfy.yaml",
	}}
	res.Next = []output.NextDo{{Reason: "set the build target", Do: "wharfy config"}}
	validateAgainst(t, configSchemaID, res)
}

// TestConfigMainDetectFailureNotInternal: go module でない dir で `wharfy config` を走らせると
// 内部の `go list ./...` が非ゼロ終了する。以前はこれが不透明な internal / "exit status 1" として
// 表面化していた(利用者報告)。今は internal で潰さず、実効設定 + main_ambiguous + 真因つき
// メッセージ + 次の一手を見せる(status が同じ Resolve を飲み込んで動くのと同じ堅さ)。
func TestConfigMainDetectFailureNotInternal(t *testing.T) {
	withTempDir(t) // go.mod も wharfy.yaml も無い空 dir → go list ./... が失敗する

	res := runConfig(context.Background(), mustLookup(t, "config"), nil)

	if res.OK {
		t.Fatal("expected ok=false when main cannot be resolved")
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected a coded problem")
	}
	if got := res.Errors[0].Code; got == output.ErrInternal {
		t.Fatalf("main detection failure must not surface as internal: %+v", res.Errors[0])
	}
	if res.Errors[0].Code != output.ErrMainAmbiguous {
		t.Errorf("code = %q, want %q", res.Errors[0].Code, output.ErrMainAmbiguous)
	}
	// 不透明な "exit status 1" ではなく go list の真因が message に残ること。
	if msg := res.Errors[0].Message; !strings.Contains(msg, "go list") {
		t.Errorf("message lost the failing command context: %q", msg)
	}
	// best-effort の実効設定を data に載せて見せること(config.json は data 必須)。
	if res.Data == nil {
		t.Error("expected effective config in data even when main is unresolved")
	}
	validateAgainst(t, configSchemaID, res)
}

// TestExecuteNonZeroOnNotOK: envelope コマンドが ok=false を返すと Execute は errNotOK を返し、
// main がこれを非ゼロ終了に変える(利用者指摘の「ok:false なのに exit 0」直し)。
func TestExecuteNonZeroOnNotOK(t *testing.T) {
	withTempDir(t) // config が main 未解決で ok=false になる dir

	root := newRootCmd()
	root.SetArgs([]string{"config"})
	err := root.Execute()
	if !errors.Is(err, errNotOK) {
		t.Fatalf("expected errNotOK from ok=false command, got %v", err)
	}
}

// TestConfigInvalidValidatesSchema: wharfy.yaml 不正時も data(推測の実効設定)を載せ、
// config.json に valid であること(data 必須を満たす)。
func TestConfigInvalidValidatesSchema(t *testing.T) {
	cfg, _ := stubResolver([]string{"./cmd/mytool"}).Resolve(config.File{})
	res := output.New("config", "wharfy.yaml is invalid (showing inferred config)", false)
	res.Data = cfg
	res.Errors = []output.Problem{{
		Code:    output.ErrConfigInvalid,
		Message: "wharfy.yaml: yaml: line 1: ...",
		Hint:    "fix wharfy.yaml; see schemas/wharfy.config.json for known keys",
	}}
	res.Next = []output.NextDo{{Reason: "fix the file then re-run", Do: "wharfy config"}}
	validateAgainst(t, configSchemaID, res)
}

// TestCommandsStopOnAnInvalidConfig: 読めない wharfy.yaml では、推測で進まず config_invalid で止まる。
// 綴り違いを黙って無視されるより悪いのは、無視された結果 build / release / publish が既定で
// 走り出すことである(設定したつもりのものが一つも効かないまま実物が出ていく)。
func TestCommandsStopOnAnInvalidConfig(t *testing.T) {
	runners := map[string]func(context.Context, registry.Command) output.Result{
		"build":   func(ctx context.Context, c registry.Command) output.Result { return runBuild(ctx, c, nil) },
		"release": func(ctx context.Context, c registry.Command) output.Result { return runRelease(ctx, c, nil) },
		"publish": func(ctx context.Context, c registry.Command) output.Result { return runPublish(ctx, c, nil) },
		"sign":    func(ctx context.Context, c registry.Command) output.Result { return runSign(ctx, c, nil) },
		"verify":  func(ctx context.Context, c registry.Command) output.Result { return runVerify(ctx, c, nil) },
	}
	for name, run := range runners {
		t.Run(name, func(t *testing.T) {
			root := scratchModule(t)
			writeConfig(t, root, "project: demo\nchannles: [homebrew]\n")
			chdir(t, root)

			res := run(context.Background(), mustLookup(t, name))
			if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrConfigInvalid {
				t.Fatalf("%s must stop on an unreadable wharfy.yaml: %+v", name, res)
			}
			if !strings.Contains(res.Errors[0].Message, `unknown key "channles"`) {
				t.Errorf("the offending key should reach the caller: %+v", res.Errors[0])
			}
		})
	}
}

// status も同じ: 推測した姿を実態として報告してはいけない。
func TestStatusStopsOnAnInvalidConfig(t *testing.T) {
	root := scratchModule(t)
	writeConfig(t, root, "verify:\n  bogus: 1\n")
	chdir(t, root)

	_, err := buildStatus(context.Background(), false)
	var invalid *config.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("status must not report an inferred config as the truth: %v", err)
	}
}
