package output

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// codes.go の drift 対策(設計 09「このカタログと実コード定数の一致をテストで担保」)。
// コードの drift も agent 出力の drift と同じ思想で防ぐ(05)。

// docCatalogPath は人間向け正準カタログ(09)。doc/wip は gitignore のため CI には無い。
// 在るとき(ローカル)だけ doc⇔code 一致を検証し、無いときは skip する。
const docCatalogPath = "../../doc/wip/wharfy/design/09_error_catalog.md"

// expectedCodes は Catalog の正準コード一覧(ソート済み)。
// CI(doc 不在)でも追加・削除を検知できるよう、コードとは別に固定しておく golden。
// 変更時はここも更新する→契約変更がレビューに乗る(05 の golden snapshot と同思想)。
var expectedCodes = []string{
	"auth_failed",
	"build_failed",
	"builder_unavailable",
	"channel_skipped",
	"config_invalid",
	"consent_required",
	"darwin_unnotarized",
	"drift_detected",
	"gated_pending",
	"github_unresolved",
	"goinstall_only_go",
	"internal",
	"main_ambiguous",
	"network_error",
	"probe_failed",
	"publish_failed",
	"tag_missing",
	"tap_will_be_created",
	"target_create_failed",
	"token_missing",
	"verify_failed",
	"win_unsigned",
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TestCatalogInternalConsistency: 重複なし・snake_case・kind が妥当。
func TestCatalogInternalConsistency(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Catalog {
		if seen[e.Code] {
			t.Errorf("duplicate code in Catalog: %q", e.Code)
		}
		seen[e.Code] = true
		if !snakeCase.MatchString(e.Code) {
			t.Errorf("code %q is not snake_case", e.Code)
		}
		if e.Kind != KindWarning && e.Kind != KindError {
			t.Errorf("code %q has invalid kind %q", e.Code, e.Kind)
		}
		if e.Summary == "" {
			t.Errorf("code %q has empty summary", e.Code)
		}
	}
}

// TestCatalogMatchesGolden: Catalog のコード集合が固定 golden と一致する(CI でも効く)。
func TestCatalogMatchesGolden(t *testing.T) {
	got := sortedCodes(catalogCodes())
	if !equalStrings(got, expectedCodes) {
		t.Errorf("Catalog codes drifted from expectedCodes.\n got: %v\nwant: %v\n(update expectedCodes if this change is intentional)", got, expectedCodes)
	}
}

// TestCatalogMatchesDoc: 09 の人間向けカタログと code 定数が一致する(doc 在るときのみ)。
func TestCatalogMatchesDoc(t *testing.T) {
	f, err := os.Open(docCatalogPath)
	if os.IsNotExist(err) {
		t.Skipf("catalog doc not present (%s); skipping doc⇔code match", docCatalogPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	docCodes := parseDocCodes(t, f)
	code := catalogCodes()

	for c, kind := range docCodes {
		k, ok := code[c]
		if !ok {
			t.Errorf("code %q is in catalog doc (09) but not in codes.go Catalog", c)
			continue
		}
		if k != kind {
			t.Errorf("code %q kind mismatch: doc=%q codes.go=%q", c, kind, k)
		}
	}
	for c := range code {
		if _, ok := docCodes[c]; !ok {
			t.Errorf("code %q is in codes.go Catalog but not in catalog doc (09)", c)
		}
	}
}

// parseDocCodes は 09 のマークダウン表から「第1列の `code`」だけを抜き、
// 直近の見出し(警告/エラー)から kind を決める。
func parseDocCodes(t *testing.T, f *os.File) map[string]CodeKind {
	t.Helper()
	firstCell := regexp.MustCompile("^`([a-z][a-z0-9_]*)`$")
	out := map[string]CodeKind{}
	var kind CodeKind
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#") && strings.Contains(line, "警告"):
			kind = KindWarning
			continue
		case strings.HasPrefix(line, "#") && strings.Contains(line, "エラー"):
			kind = KindError
			continue
		}
		if !strings.HasPrefix(line, "|") || kind == "" {
			continue
		}
		cells := strings.Split(line, "|") // [ "", cell1, cell2, ... ]
		if len(cells) < 2 {
			continue
		}
		m := firstCell.FindStringSubmatch(strings.TrimSpace(cells[1]))
		if m == nil {
			continue // ヘッダ行・区切り行・コード以外の第1列はここで落ちる
		}
		out[m[1]] = kind
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedCodes(m map[string]CodeKind) []string {
	out := make([]string, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
