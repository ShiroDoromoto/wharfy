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
