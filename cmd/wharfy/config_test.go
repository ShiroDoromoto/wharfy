package main

import (
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
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
