package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// drift テスト骨格(設計 05)。「agent が語る能力 vs 実際のコマンド実体」のズレを CI で落とす。
// コマンドを足したら registry に足すだけで全生成物に載る、を構造的に担保する。

var updateGolden = flag.Bool("update", false, "update golden snapshot files in testdata/")

const fixedTestVersion = "v0.0.0-test (0000000, 2026-06-18)"

// schemaDir / testdataDir はパッケージ(cmd/wharfy)からリポジトリ root を辿る。
const (
	schemaDir   = "../../schemas"
	testdataDir = "testdata"
)

// TestCobraMatchesRegistry: cobra 登録コマンドと registry が双方向で一致する。
// 「cobra にあるが registry にない」「registry にあるが cobra にない」をどちらも落とす。
func TestCobraMatchesRegistry(t *testing.T) {
	cobraNames := map[string]bool{}
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue // cobra が自動で足すメタコマンドは対象外
		}
		cobraNames[c.Name()] = true
	}
	regNames := registry.Names()

	for n := range cobraNames {
		if !regNames[n] {
			t.Errorf("cobra command %q is not in registry (drift: add it to registry.Commands)", n)
		}
	}
	for n := range regNames {
		if !cobraNames[n] {
			t.Errorf("registry command %q has no cobra command (drift: it won't be runnable)", n)
		}
	}
}

// TestNextReferencesExist: 全 commandSpec.next の参照先が registry に実在する(リンク切れ禁止)。
func TestNextReferencesExist(t *testing.T) {
	names := registry.Names()
	for _, c := range registry.Commands {
		for _, n := range c.Next {
			if !names[n] {
				t.Errorf("command %q next references %q which is not a registered command", c.Name, n)
			}
		}
	}
}

// TestAgentContainsAllCommands: registry の全コマンドが agent 出力の commands[] に出る。
func TestAgentContainsAllCommands(t *testing.T) {
	doc := registry.BuildAgentDoc(fixedTestVersion)
	got := map[string]bool{}
	for _, c := range doc.Commands {
		got[c.Name] = true
	}
	for n := range registry.Names() {
		if !got[n] {
			t.Errorf("registry command %q missing from agent output commands[]", n)
		}
	}
}

// TestAgentJSONValidatesSchema: agent --json 出力が schemas/agent.json に valid(02 契約)。
func TestAgentJSONValidatesSchema(t *testing.T) {
	doc := registry.BuildAgentDoc(fixedTestVersion)
	validateAgainst(t, "https://wharfy.io/schemas/v1/agent.json", doc)
}

// TestVersionJSONValidatesSchema: version の envelope が schemas/result.json に valid。
func TestVersionJSONValidatesSchema(t *testing.T) {
	res := runVersion(context.Background(), mustLookup(t, "version"), nil)
	validateAgainst(t, "https://wharfy.io/schemas/v1/result.json", res)
}

// TestStubResultValidatesSchema: 未実装スタブの envelope も契約に valid であること。
func TestStubResultValidatesSchema(t *testing.T) {
	res := stubResult(mustLookup(t, "build"))
	validateAgainst(t, "https://wharfy.io/schemas/v1/result.json", res)
}

// TestAgentGolden: agent 出力のスナップショット比較。意図しない契約変化を検知する。
// 版は固定値にして golden を安定させる(構造の差分だけを見る)。-update で更新。
func TestAgentGolden(t *testing.T) {
	doc := registry.BuildAgentDoc(fixedTestVersion)
	got, err := output.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join(testdataDir, "agent.golden.json")
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run `go test ./cmd/wharfy -update` to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("agent output drifted from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// --- helpers ---

func mustLookup(t *testing.T, name string) registry.Command {
	t.Helper()
	c, ok := registry.Lookup(name)
	if !ok {
		t.Fatalf("registry has no command %q", name)
	}
	return c
}

// validateAgainst は v を JSON 化して、指定 $id のスキーマで検証する。
// schemas/ の全ファイルを $id で登録するので $ref はローカル解決(ネットワーク不要)。
func validateAgainst(t *testing.T, schemaID string, v any) {
	t.Helper()
	c := jsonschema.NewCompiler()
	files, err := filepath.Glob(filepath.Join(schemaDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		id, doc := readSchema(t, f)
		if id == "" {
			continue // common.json 等も $id を持つ。無いものは登録不要
		}
		if err := c.AddResource(id, doc); err != nil {
			t.Fatalf("add resource %s: %v", f, err)
		}
	}
	sch, err := c.Compile(schemaID)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaID, err)
	}

	s, err := output.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("output is not valid against %s:\n%v\n--- output ---\n%s", schemaID, err, s)
	}
}

func readSchema(t *testing.T, path string) (id string, doc any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err = jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if m, ok := doc.(map[string]any); ok {
		if v, ok := m["$id"].(string); ok {
			id = v
		}
	}
	return id, doc
}
