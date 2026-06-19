package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// TestSignWiring: sign は advisory な状態を返し、windows 未署名を win_unsigned で見せ、
// ok:true でも「署名した」と誤認させない(message が advisory を明示)。
func TestSignWiring(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)

	res := runSign(context.Background(), mustLookup(t, "sign"), nil)
	if !res.OK {
		t.Fatalf("sign should not block (advisory): %+v", res)
	}
	if !strings.Contains(res.Message, "advisory") {
		t.Errorf("message must make clear signing was not performed: %q", res.Message)
	}
	if !hasWarning(res, output.WarnWinUnsigned) {
		t.Errorf("windows unsigned should warn win_unsigned: %+v", res.Warnings)
	}
	// next は「unsigned でも publish できる」へ導く(no-op の export 偽装をしない)。
	if !hasNextDo(res, "wharfy publish homebrew") {
		t.Errorf("sign next should allow continuing to publish: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

func hasWarning(res output.Result, code string) bool {
	for _, w := range res.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
