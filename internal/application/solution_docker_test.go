package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestSolutionExecutorRealDockerVerificationAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	for _, mode := range []string{"pass", "wrong_answer", "compile_error", "compile_receipt_gap", "run_receipt_gap", "ce_receipt_gap"} {
		t.Run(mode, func(t *testing.T) {
			contentMode := mode
			if mode == "ce_receipt_gap" {
				contentMode = "compile_error"
			}
			outputs := solutionDockerOutputs(t, contentMode)
			f, service := newSolutionExecutorFixtureWithOutputs(t, false, outputs)
			input, content, view := beginCommittedSolutionVerification(t, f, service)
			gap := &sandboxReceiptGapStore{Store: f.store, kind: domain.CallSandboxCompile}
			if mode == "run_receipt_gap" {
				gap.kind = domain.CallSandboxRun
			}
			factory := func(_ context.Context, identity port.SandboxAuthorizationIdentity) (application.SolutionSandbox, toolchain.Lock, error) {
				config := base
				config.Store, config.Blobs, config.Clock, config.Identity = f.store, f.executorConfig.Blobs, clock.Real{}, identity
				if strings.HasSuffix(mode, "receipt_gap") {
					config.Store = gap
				}
				worker, err := application.NewDockerSandboxSession(config)
				return worker, config.Lock, err
			}
			result, err := service.VerifyDraft(ctx, view, factory)
			if strings.HasSuffix(mode, "receipt_gap") {
				if !errors.Is(err, errInjectedSandboxReceiptGap) || !gap.failed.Load() {
					t.Fatalf("receipt interruption was not reached: %v", err)
				}
				service, err = application.NewSolutionExecutor(f.service)
				if err != nil {
					t.Fatal(err)
				}
				result, err = service.VerifyDraft(ctx, view, factory)
			}
			wantPass := mode != "wrong_answer" && contentMode != "compile_error"
			if err != nil || result.Report.ValidateFor(input, content) != nil || result.Report.Passed != wantPass {
				t.Fatalf("real verification=%+v %v", result.Report, err)
			}
			before, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := application.NewSolutionExecutor(f.service)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := restarted.VerifyDraft(ctx, view, factory)
			beforeReport, _ := json.Marshal(result.Report)
			afterReport, _ := json.Marshal(replay.Report)
			beforeArtifacts, _ := json.Marshal(result.Occurrences)
			afterArtifacts, _ := json.Marshal(replay.Occurrences)
			if err != nil || !bytes.Equal(beforeReport, afterReport) || !bytes.Equal(beforeArtifacts, afterArtifacts) {
				t.Fatalf("verification replay changed report=%v or attachment evidence=%v: %v", !bytes.Equal(beforeReport, afterReport), !bytes.Equal(beforeArtifacts, afterArtifacts), err)
			}
			after, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil || !reflect.DeepEqual(before.Remaining, after.Remaining) || f.httpCalls.Load() != 3 || f.sends.Load() != 1 {
				t.Fatalf("replay consumed budget or dispatched providers: before=%+v after=%+v %v", before, after, err)
			}
			wantContainers := int64(8 + 4*len(input.Problem.Samples))
			if mode == "wrong_answer" {
				wantContainers = 10
			}
			if contentMode == "compile_error" {
				wantContainers = 3 // no binary promotion after CE
			}
			if used := int64(100) - after.Remaining[domain.BudgetDockerContainerCreates]; used != wantContainers {
				t.Fatalf("unexpected physical Docker dispatch/retry: used=%d want=%d", used, wantContainers)
			}
			if _, err := restarted.ReadVerification(ctx, f.runID, base); err == nil {
				t.Fatal("uncommitted verification artifacts became a passing gate")
			}
			f.finish(t, view, replay.ReportArtifact.Blob.Digest, "solution_checkpoint", replay.ReportArtifact.Blob.Digest, replay.Occurrences)
			committed, err := restarted.ReadVerification(ctx, f.runID, base)
			if err != nil {
				t.Fatalf("read committed Docker verification: %v", err)
			}
			readReport, _ := json.Marshal(committed)
			if !bytes.Equal(readReport, afterReport) {
				t.Fatal("committed report differs from verification")
			}
			wrongConfig := base
			wrongConfig.Limits.HelperPIDs++
			if _, err := restarted.ReadVerification(ctx, f.runID, wrongConfig); err == nil {
				t.Fatal("changed sandbox policy reused verification evidence")
			}
			if mode == "pass" {
				assertCommittedSolutionRejectsAlteredStage(t, ctx, f, base)
			}
			t.Logf("committed real solution verification: passed=%v reason=%s compiles=%d samples=%d containers=%d artifacts=%d", replay.Report.Passed, replay.Report.Reason, len(replay.Report.Compiles), len(replay.Report.Samples), wantContainers, len(replay.Occurrences))
		})
	}
}

var errInjectedSandboxReceiptGap = errors.New("injected interruption before sandbox result publication")

type sandboxReceiptGapStore struct {
	*sqlite.Store
	kind   domain.CallKind
	failed atomic.Bool
}

func (s *sandboxReceiptGapStore) CreateArtifactDeclaration(ctx context.Context, declaration domain.ArtifactDeclarationRecord) error {
	if strings.HasSuffix(string(declaration.LogicalPath), "/result.json") {
		call, err := s.Store.ReadLogicalCall(ctx, declaration.CallRecordID)
		if err != nil {
			return err
		}
		if call.Kind == s.kind && s.failed.CompareAndSwap(false, true) {
			return errInjectedSandboxReceiptGap
		}
	}
	return s.Store.CreateArtifactDeclaration(ctx, declaration)
}

type alteredSolutionStageStore struct {
	*sqlite.Store
	alter func(*port.CommittedPrivateStage)
}

func (s alteredSolutionStageStore) ReadCommittedSandboxStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedPrivateStage, error) {
	result, err := s.Store.ReadCommittedSandboxStage(ctx, runID, stage)
	if err == nil {
		s.alter(&result)
	}
	return result, err
}

func assertCommittedSolutionRejectsAlteredStage(t *testing.T, ctx context.Context, f *similarityExecutorFixture, config application.DockerSandboxConfig) {
	t.Helper()
	for name, alter := range map[string]func(*port.CommittedPrivateStage){
		"changed input": func(s *port.CommittedPrivateStage) { s.Attempt.InputDigest = domain.SumBytes([]byte("other draft")) },
		"missing source": func(s *port.CommittedPrivateStage) {
			for i, a := range s.Artifacts {
				if a.Blob.Role == domain.ArtifactSource {
					s.Artifacts = append(s.Artifacts[:i], s.Artifacts[i+1:]...)
					break
				}
			}
		},
		"missing stdout": func(s *port.CommittedPrivateStage) {
			for i, a := range s.Artifacts {
				if a.Blob.Role == domain.ArtifactStdout {
					s.Artifacts = append(s.Artifacts[:i], s.Artifacts[i+1:]...)
					break
				}
			}
		},
		"extra artifact": func(s *port.CommittedPrivateStage) {
			extra := s.Artifacts[0]
			extra.Blob.LogicalPath = "unrelated.json"
			s.Artifacts = append(s.Artifacts, extra)
		},
	} {
		t.Run(name, func(t *testing.T) {
			generationConfig := f.executorConfig
			generationConfig.Store = alteredSolutionStageStore{f.store, alter}
			generation, err := application.NewGenerationExecutor(generationConfig)
			if err != nil {
				t.Fatal(err)
			}
			similarityConfig := f.config
			similarityConfig.Generation = generation
			evidence, err := application.NewSimilarityExecutor(similarityConfig)
			if err != nil {
				t.Fatal(err)
			}
			service, err := application.NewSolutionExecutor(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.ReadVerification(ctx, f.runID, config); err == nil {
				t.Fatal("altered committed verification was accepted")
			}
		})
	}
}

// The model and Similarity endpoints remain local HTTP fixtures. Only the
// compiler/program execution below uses actual pinned Docker images.
func solutionDockerOutputs(t *testing.T, mode string) map[string][]byte {
	t.Helper()
	outputs := llmBuiltinOutputs(t)
	statement := domain.StatementDraftV1{SchemaVersion: domain.StatementDraftSchemaV1, Title: "Distances from vertex 1", Description: "Given an undirected unweighted graph, find the shortest distance from vertex 1 to every vertex. Vertices are numbered from 1 to n.", Input: domain.ProblemIO{Description: "The first line contains n and m (1 <= n <= 100, 0 <= m <= n*(n-1)/2). The next m lines contain the two endpoints of an edge.", Fields: []string{"n", "m", "edges"}}, Output: domain.ProblemIO{Description: "Print n integers in vertex order. Print -1 for unreachable vertices.", Fields: []string{"distances"}}, Samples: []domain.ProblemSample{{Input: "1 0\n", Output: "0\n"}, {Input: "5 3\n1 2\n2 3\n1 4\n", Output: "0 1 2 1 -1\n"}}}
	reference := `#include <iostream>
#include <vector>
#include <queue>
int main(){int n,m;if(!(std::cin>>n>>m))return 1;std::vector<std::vector<int>>g(n);while(m--){int u,v;std::cin>>u>>v;--u;--v;g[u].push_back(v);g[v].push_back(u);}std::vector<int>d(n,-1);std::queue<int>q;d[0]=0;q.push(0);while(!q.empty()){int u=q.front();q.pop();for(int v:g[u])if(d[v]<0){d[v]=d[u]+1;q.push(v);}}for(int i=0;i<n;++i)std::cout<<d[i]<<' ';std::cout<<std::endl;}
`
	brute := `#include <iostream>
#include <vector>
#include <algorithm>
int main(){int n,m;if(!(std::cin>>n>>m))return 1;std::vector<std::vector<int>>d(n,std::vector<int>(n,10000));for(int i=0;i<n;++i)d[i][i]=0;while(m--){int u,v;std::cin>>u>>v;--u;--v;d[u][v]=d[v][u]=1;}for(int k=0;k<n;++k)for(int i=0;i<n;++i)for(int j=0;j<n;++j)d[i][j]=std::min(d[i][j],d[i][k]+d[k][j]);for(int i=0;i<n;++i)std::cout<<(d[0][i]==10000?-1:d[0][i])<<' ';std::cout<<std::endl;}
`
	if mode == "wrong_answer" {
		reference = "#include <iostream>\nint main(){std::cout<<42;}\n"
	}
	if mode == "compile_error" {
		reference = "int main( {\n"
	}
	solution := domain.SolutionDraftV1{SchemaVersion: domain.SolutionDraftSchemaV1, ReferenceCode: reference, BruteCode: brute, Explanation: "Reference uses BFS from vertex 1 in O(n+m). The small-instance oracle uses Floyd-Warshall in O(n^3) independently."}
	for name, value := range map[string]any{"statement.draft": statement, "solution.draft": solution} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		outputs[name] = raw
	}
	return outputs
}
