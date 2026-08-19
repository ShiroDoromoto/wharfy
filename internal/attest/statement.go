package attest

// statement.go — 来歴そのもの(in-toto の証言)を組み立てる。
//
// 形は SLSA Provenance v1 / GitHub Actions build type。actions/attest-build-provenance が出すものと
// 同じ形にするのは趣味ではなく必要で、受け取る側の `gh attestation verify` はこの形を前提に
// 「どの workflow が作ったか」を読む。ここが独自形式だと、証明は付いているのに誰も検算できない。

import (
	"encoding/json"
	"errors"
	"strings"
)

// in-toto / SLSA の型 URI。値そのものが対外契約なのでここが単一真実。
const (
	statementType   = "https://in-toto.io/Statement/v1"
	predicateType   = "https://slsa.dev/provenance/v1"
	actionsBuildTyp = "https://actions.github.io/buildtypes/workflow/v1"
	// PayloadType は DSSE の payloadType(bundle に載る)。
	PayloadType = "application/vnd.in-toto+json"
)

// Env は来歴に載せる GitHub Actions の文脈。Actions が env で配るものだけで組み立てる
// (推測しない——推測した来歴は来歴ではない)。
type Env struct {
	Repository      string // owner/repo (GITHUB_REPOSITORY)
	RepositoryID    string // GITHUB_REPOSITORY_ID
	RepositoryOwner string // GITHUB_REPOSITORY_OWNER_ID
	ServerURL       string // GITHUB_SERVER_URL (既定 https://github.com)
	SHA             string // GITHUB_SHA — この commit から出た、と言い切る根拠
	Ref             string // GITHUB_REF (refs/tags/vX.Y.Z)
	WorkflowRef     string // GITHUB_WORKFLOW_REF — 起動された workflow(owner/repo/.github/workflows/release.yml@refs/tags/vX.Y.Z)
	JobWorkflowRef  string // OIDC の job_workflow_ref — この job を抱えている workflow(＝署名者。env には出ない)
	EventName       string // GITHUB_EVENT_NAME
	RunID           string // GITHUB_RUN_ID
	RunAttempt      string // GITHUB_RUN_ATTEMPT
	RunnerEnv       string // RUNNER_ENVIRONMENT (github-hosted / self-hosted)
}

// errNoWorkflow は来歴の要(どの workflow の何回目の run が作ったか)が env に無いとき。
// ここを埋められないなら証明を作ってはいけない——「作ったのは誰か」を空欄にした来歴は、
// 検算する側から見て何も言っていないのと同じ。
var errNoWorkflow = errors.New("attest: GITHUB_WORKFLOW_REF / GITHUB_SHA are required to describe who built this")

// Statement は subjects に対する SLSA provenance の証言(DSSE の payload)を JSON で返す。
func Statement(subjects []Subject, env Env) ([]byte, error) {
	if env.WorkflowRef == "" || env.SHA == "" {
		return nil, errNoWorkflow
	}
	server := env.ServerURL
	if server == "" {
		server = "https://github.com"
	}

	subs := make([]map[string]any, 0, len(subjects))
	for _, s := range subjects {
		subs = append(subs, map[string]any{
			"name":   s.Name,
			"digest": map[string]string{"sha256": s.SHA256},
		})
	}

	// builder.id は「誰が署名したか」＝この job を抱えている workflow。GitHub の attestations API は
	// ここを証明書の Build Signer URI(job_workflow_ref)と突き合わせ、食い違えば 422 で預かりを拒む。
	// 入口(release.yml)が実体(_release.yml)を uses: で呼ぶ構成では、env の GITHUB_WORKFLOW_REF は
	// 入口を指すので使えない。一方 buildDefinition の workflow は「何が起動されたか」だから、
	// そちらは入口のままが正しい。JobWorkflowRef が空なら両者は同じ(＝割っていない構成)。
	signer := env.JobWorkflowRef
	if signer == "" {
		signer = env.WorkflowRef
	}

	stmt := map[string]any{
		"_type":         statementType,
		"subject":       subs,
		"predicateType": predicateType,
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": actionsBuildTyp,
				"externalParameters": map[string]any{
					"workflow": map[string]any{
						"ref":        refOf(env.WorkflowRef),
						"repository": server + "/" + env.Repository,
						"path":       workflowPath(env.WorkflowRef, env.Repository),
					},
				},
				"internalParameters": map[string]any{
					"github": map[string]any{
						"event_name":          env.EventName,
						"repository_id":       env.RepositoryID,
						"repository_owner_id": env.RepositoryOwner,
						"runner_environment":  env.RunnerEnv,
					},
				},
				// 何から作ったか。tag ではなく commit を digest で名指す(tag は動かせるが commit は動かない)。
				"resolvedDependencies": []map[string]any{{
					"uri":    "git+" + server + "/" + env.Repository + "@" + env.Ref,
					"digest": map[string]string{"gitCommit": env.SHA},
				}},
			},
			"runDetails": map[string]any{
				"builder": map[string]any{"id": server + "/" + signer},
				"metadata": map[string]any{
					"invocationId": server + "/" + env.Repository + "/actions/runs/" + env.RunID + "/attempts/" + env.RunAttempt,
				},
			},
		},
	}
	return json.Marshal(stmt)
}

// refOf は workflow_ref (owner/repo/path@ref) の ref 側を返す。
func refOf(workflowRef string) string {
	if i := strings.LastIndex(workflowRef, "@"); i >= 0 {
		return workflowRef[i+1:]
	}
	return ""
}

// workflowPath は workflow_ref から repo 部分と ref を落として、workflow ファイルの repo 相対パスを返す。
func workflowPath(workflowRef, repository string) string {
	p := workflowRef
	if i := strings.LastIndex(p, "@"); i >= 0 {
		p = p[:i]
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, repository), "/")
}
