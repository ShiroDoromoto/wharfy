package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// recordPublish は state に homebrew 発行記録を書く(verify の前提)。
func recordPublish(t *testing.T, root, version string) {
	t.Helper()
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"homebrew": {Version: version, Target: "acme/homebrew-demo", At: "t"},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
}

func plantFormula(version string) *channel.InMemoryTapStore {
	s := channel.NewInMemoryTapStore()
	s.Files["Formula/demo.rb"] = "class Demo < Formula\n  version \"" + version + "\"\nend\n"
	return s
}

// 未発行 → 確認対象なしを正直に返し、publish へ導く(空 next の dead-end を作らない)。
func TestVerifyNothingPublished(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	defer swapTapStore(channel.NewInMemoryTapStore())()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("nothing-to-verify is not a failure: %+v", res)
	}
	if len(res.Next) == 0 || !hasNextDo(res, "wharfy publish homebrew --yes") {
		t.Errorf("verify must guide to publish, not dead-end: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 発行済み＋tap の版が一致 → verified。
func TestVerifyMatch(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(plantFormula("1.2.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if !res.OK {
		t.Fatalf("matching version should verify ok: %+v", res)
	}
	if !hasNextDo(res, "wharfy status") {
		t.Errorf("verified next should point to status: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 発行記録あり・tap に formula 無し → verify_failed。
func TestVerifyMissingFormula(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(channel.NewInMemoryTapStore())() // tap 空

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("missing formula should be verify_failed: %+v", res)
	}
}

// 発行記録と tap の版が食い違い → verify_failed。
func TestVerifyVersionMismatch(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	recordPublish(t, root, "1.2.0")
	defer swapTapStore(plantFormula("1.1.0"))()

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("version mismatch should be verify_failed: %+v", res)
	}
}
