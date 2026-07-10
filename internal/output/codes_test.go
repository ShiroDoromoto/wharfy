package output

import (
	"regexp"
	"sort"
	"testing"
)

// codes.go の drift 対策。コードの drift も agent 出力の drift と同じ思想で防ぐ。

// expectedCodes は Catalog の正準コード一覧(ソート済み)。
// 追加・削除を検知できるよう、コードとは別に固定しておく golden。
// 変更時はここも更新する→契約変更がレビューに乗る(agent の golden snapshot と同思想)。
var expectedCodes = []string{
	"auth_failed",
	"build_failed",
	"builder_unavailable",
	"channel_skipped",
	"checksum_mismatch",
	"config_invalid",
	"consent_required",
	"darwin_unnotarized",
	"deprecate_frozen",
	"deprecate_no_notice_surface",
	"deprecate_orphan",
	"drift_detected",
	"gated_pending",
	"github_unresolved",
	"goinstall_only_go",
	"init_missing",
	"init_write_failed",
	"internal",
	"keychain_failed",
	"main_ambiguous",
	"network_error",
	"probe_failed",
	"publish_failed",
	"sign_failed",
	"stale_generator",
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
