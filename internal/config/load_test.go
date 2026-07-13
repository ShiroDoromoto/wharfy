package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// load_test.go — wharfy.yaml の読み込み。書いたキーは効くか断られるかのどちらかである。

// writeYAML は root に wharfy.yaml を置く。
func writeYAML(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 未知キーは黙って無視されない。綴りを間違えた配布者は、既定で走り出す前に止められる。
func TestLoadRejectsAnUnknownTopLevelKey(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "project: demo\nchannles: [homebrew]\n")

	_, err := Load(root)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("a misspelled key must be an InvalidError: %v", err)
	}
	if !strings.Contains(err.Error(), `unknown key "channles"`) {
		t.Errorf("the message should name the key that is not known: %v", err)
	}
	if !strings.Contains(err.Error(), ConfigFileName) {
		t.Errorf("the message should name the file: %v", err)
	}
}

// ブロックの中の未知キーは、そのブロック名とともに言う(#31 の verify.images / verify.run と同じ層)。
func TestLoadNamesTheBlockOfAnUnknownKey(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "project: demo\nverify:\n  bogus: 1\n")

	_, err := Load(root)
	if err == nil {
		t.Fatal("an unknown key under verify: must be rejected")
	}
	if !strings.Contains(err.Error(), `unknown key "bogus" in verify:`) {
		t.Errorf("the message should say which block the key was written in: %v", err)
	}
	if strings.Contains(err.Error(), "VerifyInput") {
		t.Errorf("the distributor wrote yaml, not Go structs; do not leak type names: %v", err)
	}
}

// 同じ型が 2 つのブロックから使われている(apt/rpm の RepoInput)なら、どちらとも言えない。
// 嘘のブロック名を出すくらいなら行番号だけ出す。
func TestLoadDoesNotGuessAnAmbiguousBlock(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "rpm:\n  provider: fury\n  usr: shirodoromoto\n")

	_, err := Load(root)
	if err == nil {
		t.Fatal("an unknown key under rpm: must be rejected")
	}
	if !strings.Contains(err.Error(), `line 3: unknown key "usr"`) {
		t.Errorf("the line should locate the key: %v", err)
	}
	if strings.Contains(err.Error(), " in ") {
		t.Errorf("apt and rpm share a type, so no block can be named: %v", err)
	}
}

// 未知キーが複数あれば、1 回で全部言う(直しては叱られる往復をさせない)。
func TestLoadReportsEveryUnknownKey(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "projct: demo\nlicence: MIT\n")

	_, err := Load(root)
	if err == nil {
		t.Fatal("both misspelled keys must be rejected")
	}
	for _, key := range []string{"projct", "licence"} {
		if !strings.Contains(err.Error(), `unknown key "`+key+`"`) {
			t.Errorf("every unknown key should be reported at once, %q is missing: %v", key, err)
		}
	}
}

// 構文の誤りも InvalidError(推測で進めてはいけない点は未知キーと同じ)。
func TestLoadRejectsBrokenYAML(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "project: [unclosed\n")

	_, err := Load(root)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("broken yaml must be an InvalidError: %v", err)
	}
}

// コメントだけのファイルは空の File(wharfy init が置く雛形が全部コメントでも動く)。
func TestLoadAcceptsAFileWithOnlyComments(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "# nothing declared yet\n")

	f, err := Load(root)
	if err != nil {
		t.Fatalf("a file with only comments is an empty declaration, not an error: %v", err)
	}
	if f.Project != "" {
		t.Errorf("nothing was declared: %+v", f)
	}
}

// 既知のキーは今までどおり読める(strict にして既存の宣言が壊れていない)。
func TestLoadAcceptsEveryKnownBlock(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, `project: demo
channels: [homebrew, apt, rpm]
apt:
  provider: fury
  user: someone
rpm:
  provider: fury
  user: someone
verify:
  images:
    apt: ubuntu:24.04
  run: [status, --quiet]
deprecate:
  scoop:
    since: "1.0.0"
    ship: false
    message: use winget
runtime_deps:
  - name: git
    min: "2.0"
    as:
      apt: git-core
`)

	f, err := Load(root)
	if err != nil {
		t.Fatalf("every key here is declared in File: %v", err)
	}
	if f.Verify == nil || f.Verify.Images["apt"] != "ubuntu:24.04" || len(f.Verify.Run) != 2 {
		t.Errorf("verify should be read: %+v", f.Verify)
	}
	if f.Deprecate["scoop"] == nil || f.Deprecate["scoop"].Ship == nil || *f.Deprecate["scoop"].Ship {
		t.Errorf("deprecate should be read: %+v", f.Deprecate)
	}
	if len(f.RuntimeDeps) != 1 || f.RuntimeDeps[0].As["apt"] != "git-core" {
		t.Errorf("runtime_deps should be read: %+v", f.RuntimeDeps)
	}
}

// latest_json.extra の中だけは wharfy の語彙ではない —— 未知キー拒否は効かず、値の形も選ばない。
func TestLoadReadsLatestJSONExtraVerbatim(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, `project: demo
latest_json:
  extra:
    store_format: 5
    min_app_version: "1.4.0"
    sunset:
      version: "2.0"
      drops_below: 5
    channels: [stable, beta]
`)

	f, err := Load(root)
	if err != nil {
		t.Fatalf("extra takes any JSON value: %v", err)
	}
	if f.LatestJSON == nil {
		t.Fatal("latest_json should be read")
	}
	extra := f.LatestJSON.Extra
	if extra["store_format"] != 5 {
		t.Errorf("a number must stay a number: %#v", extra["store_format"])
	}
	if extra["min_app_version"] != "1.4.0" {
		t.Errorf("min_app_version = %#v", extra["min_app_version"])
	}
	// 入れ子は json 化できる形(map[string]any)まで直っている —— yaml.v3 の map[any]any のままだと
	// latest.json の生成が黙って落ちる。
	nested, ok := extra["sunset"].(map[string]any)
	if !ok {
		t.Fatalf("a nested map must be jsonable: %#v", extra["sunset"])
	}
	if nested["drops_below"] != 5 {
		t.Errorf("nested value = %#v", nested["drops_below"])
	}
	if list, ok := extra["channels"].([]any); !ok || len(list) != 2 {
		t.Errorf("a list must survive: %#v", extra["channels"])
	}
}

// 外側の latest_json: は今までどおり厳格(自由なのは extra の中だけ)。
func TestLoadRejectsAnUnknownKeyBesideExtra(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "project: demo\nlatest_json:\n  extar:\n    a: 1\n")

	_, err := Load(root)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("a misspelled key beside extra must be refused: %v", err)
	}
	if !strings.Contains(err.Error(), `unknown key "extar"`) {
		t.Errorf("the message should name the key: %v", err)
	}
}

// JSON のオブジェクトキーは文字列だけ。YAML では書けてしまうので、書いた場所で断る。
func TestLoadRejectsANonStringKeyInExtra(t *testing.T) {
	root := t.TempDir()
	writeYAML(t, root, "project: demo\nlatest_json:\n  extra:\n    sunset:\n      5: drop\n")

	_, err := Load(root)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("a non-string key cannot be JSON: %v", err)
	}
	if !strings.Contains(err.Error(), "latest_json.extra.sunset") {
		t.Errorf("the message should point at where it was written: %v", err)
	}
}
