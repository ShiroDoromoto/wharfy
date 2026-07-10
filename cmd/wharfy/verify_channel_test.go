package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// verify_channel_test.go — `wharfy verify [channel]` は対象を 1 チャネルに絞る。
//
// apt/rpm はコンテナを起こすので、1 チャネルを直している間に他まで走らせると反復が重い。
// 名指しの集合は publish と同じく channels: に閉じる(D-4)。

// recordPublishAll は複数チャネル分の発行記録を書く(絞り込みの前後を同条件にするため)。
func recordPublishAll(t *testing.T, root string, recs map[string]state.PublishRecord) {
	t.Helper()
	st, _ := state.Load(root, "demo")
	st.Publish = recs
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
}

// scratchAptAndHomebrew は apt と homebrew を宣言し、両方を発行済みにした作業ツリーを作る。
// tap には formula を置かない —— homebrew を検証したら必ず failed になる仕掛けで、
// 「名指ししたチャネルだけが走った」ことを ok で証明できる。
func scratchAptAndHomebrew(t *testing.T, repo string) string {
	t.Helper()
	root := scratchModule(t)
	writeConfig(t, root, "project: demo\nchannels: [homebrew, apt]\napt:\n  repo: "+repo+"\n")
	recordPublishAll(t, root, map[string]state.PublishRecord{
		"homebrew": {Version: "1.2.0", Target: "acme/homebrew-demo", At: "t"},
		"apt":      {Version: "1.2.0", Target: repo, At: "t"},
	})
	return root
}

// `verify apt` は apt だけを検証する。homebrew の tap は空なので、走れば failed になる。
func TestVerifySingleChannelSkipsTheOthers(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchAptAndHomebrew(t, srv.URL))
	defer swapTapStore(channel.NewInMemoryTapStore())()
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), []string{"apt"})

	if !res.OK {
		t.Fatalf("naming apt must verify apt alone and pass: %+v", res)
	}
	ck := checksOf(t, res)
	if len(ck) != 1 || ck[0].Channel != "apt" {
		t.Fatalf("only the named channel belongs in checks: %+v", ck)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 引数なしなら従来どおり全チャネル(絞り込みが既定を変えていないことの証)。
func TestVerifyWithoutArgCoversEveryChannel(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchAptAndHomebrew(t, srv.URL))
	defer swapTapStore(channel.NewInMemoryTapStore())()
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("demo 1.2.0"), nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), nil)

	if len(checksOf(t, res)) != 2 {
		t.Fatalf("no argument means every configured channel: %+v", checksOf(t, res))
	}
	if res.OK {
		t.Errorf("the empty tap must fail homebrew when it is verified: %+v", res)
	}
}

// channels: に無いチャネルの名指しは検証せず拒む(publish と同じ語彙)。
func TestVerifyRejectsUnconfiguredChannel(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))
	swapDocker(t, true, func(_ context.Context, _ ...string) ([]byte, error) {
		t.Error("no container may run for a channel that is not configured")
		return nil, nil
	})

	res := runVerify(context.Background(), mustLookup(t, "verify"), []string{"cask"})

	if res.OK {
		t.Fatalf("verifying a channel absent from channels: must fail: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrChannelNotConfigured {
		t.Fatalf("want channel_not_configured: %+v", res.Errors)
	}
	if len(checksOf(t, res)) != 0 {
		t.Errorf("nothing was verified, so no check may be reported: %+v", checksOf(t, res))
	}
	// 宣言した集合を見せる(何を検証できるかが分かる)。
	if !strings.Contains(res.Errors[0].Message, "apt") {
		t.Errorf("error should name the declared channels: %q", res.Errors[0].Message)
	}
	validateAgainst(t, resultSchemaID, res)
}

// 綴り違いも「設定に無い」に落ちるが、書き戻せとは言わない(そんなチャネルは存在しない)。
func TestVerifyUnknownChannelHintsAtTheSpelling(t *testing.T) {
	srv := aptRepoServer(t, "demo", "1.2.0")
	chdir(t, scratchLinuxRepo(t, "apt", srv.URL, "1.2.0"))

	res := runVerify(context.Background(), mustLookup(t, "verify"), []string{"aptt"})

	if res.OK || len(res.Errors) == 0 {
		t.Fatalf("an unknown channel must fail: %+v", res)
	}
	if !strings.Contains(res.Errors[0].Hint, "spelling") {
		t.Errorf("a typo cannot be fixed by editing channels:: %q", res.Errors[0].Hint)
	}
}
