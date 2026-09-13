package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

func TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	for _, mode := range []string{"pass", "wrong_answer", "invalid_generated", "nondeterministic", "differential_wa", "reference_tle"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newSolutionExecutorFixtureForWorkflow(t, false, dataDockerOutputs(t, mode), workflow.GenerationRevision)
			generationConfig := f.executorConfig
			generationConfig.Clock = clock.Real{}
			var gap *dataReportGapStore
			if mode == "pass" {
				gap = &dataReportGapStore{Store: f.store}
				generationConfig.Store = gap
			}
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
			solution, err := application.NewSolutionExecutor(evidence, generation)
			if err != nil {
				t.Fatal(err)
			}
			data, err := application.NewDataExecutor(solution, base)
			if err != nil {
				t.Fatal(err)
			}
			quality, err := application.NewQualityExecutor(data)
			if err != nil {
				t.Fatal(err)
			}
			_, effective, err := f.store.RunViewDocuments(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			newService := func() *application.LocalRunService {
				service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: evidence, Reviews: f.store, ActiveTimeInterval: time.Second, EffectiveConfigJSON: effective, SolutionSandbox: &base})
				if err != nil {
					t.Fatal(err)
				}
				return service
			}
			request := domain.RunRequest(f.snapshot.Request)
			seed := f.snapshot.EffectiveSeed
			request.Seed = &seed
			result, err := newService().Generate(ctx, request)
			if gap != nil {
				if !errors.Is(err, errDataReportGap) || !gap.fired.Load() || result.CurrentStage != "data_verify" {
					t.Fatalf("missing data report interruption: %+v %v", result, err)
				}
				before, readErr := f.store.CurrentStageAttempt(ctx, result.RunID, "data_verify")
				if readErr != nil {
					t.Fatal(readErr)
				}
				result, err = newService().Resume(ctx, result.RunID)
				after, readErr := f.store.CurrentStageAttempt(ctx, result.RunID, "data_verify")
				if readErr != nil || before.AttemptID != after.AttemptID || after.Ordinal != 1 {
					t.Fatalf("data report recovery replaced the original attempt: %+v %v", after, readErr)
				}
				if !errors.Is(err, errJudgeReportGap) || !gap.judgeFired.Load() || result.CurrentStage != "judge" {
					dataReport, dataReportErr := data.Reader().ReadVerification(ctx, result.RunID)
					report, reportErr := data.Reader().ReadJudgeVerification(ctx, result.RunID)
					t.Fatalf("missing Judge report interruption: %+v %v; data reason=%q, data report error=%v; judge reason=%q, judge report error=%v", result, err, dataReport.Reason, dataReportErr, report.Reason, reportErr)
				}
				before, readErr = f.store.CurrentStageAttempt(ctx, result.RunID, "judge")
				if readErr != nil {
					t.Fatal(readErr)
				}
				result, err = newService().Resume(ctx, result.RunID)
				after, readErr = f.store.CurrentStageAttempt(ctx, result.RunID, "judge")
				if readErr != nil || before.AttemptID != after.AttemptID || after.Ordinal != 1 {
					t.Fatalf("Judge recovery replaced its attempt: %+v %v", after, readErr)
				}
				if !errors.Is(err, errQualityReportGap) || !gap.qualityFired.Load() || result.CurrentStage != "quality" {
					t.Fatalf("missing quality report interruption: %+v %v", result, err)
				}
				before, readErr = f.store.CurrentStageAttempt(ctx, result.RunID, "quality")
				if readErr != nil {
					t.Fatal(readErr)
				}
				result, err = newService().Resume(ctx, result.RunID)
				after, readErr = f.store.CurrentStageAttempt(ctx, result.RunID, "quality")
				if readErr != nil || before.AttemptID != after.AttemptID || after.Ordinal != 1 {
					t.Fatalf("quality recovery replaced its attempt: %+v %v", after, readErr)
				}
				if !errors.Is(err, errPackageCommitGap) || result.CurrentStage != "package" || result.State != domain.RunRunning {
					t.Fatalf("missing package commit interruption: %+v %v", result, err)
				}
				if _, err := f.store.ReadVerifiedPackage(ctx, result.RunID); err == nil {
					t.Fatal("uncommitted archive acquired READY authority")
				}
				packageAttempt, readErr := f.store.CurrentStageAttempt(ctx, result.RunID, "package")
				if readErr != nil {
					t.Fatal(readErr)
				}
				budgetBefore, readErr := f.store.BudgetSnapshot(ctx, result.RunID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				result, err = newService().Resume(ctx, result.RunID)
				packageAfter, readErr := f.store.CurrentStageAttempt(ctx, result.RunID, "package")
				if readErr != nil || packageAfter.AttemptID != packageAttempt.AttemptID || packageAfter.Ordinal != 1 {
					t.Fatal("package recovery replaced its attempt")
				}
				budgetAfter, readErr := f.store.BudgetSnapshot(ctx, result.RunID)
				if readErr != nil || budgetAfter.Remaining[domain.BudgetArtifactPhysicalNewBytes] != budgetBefore.Remaining[domain.BudgetArtifactPhysicalNewBytes] || budgetAfter.Remaining[domain.BudgetDockerContainerCreates] != budgetBefore.Remaining[domain.BudgetDockerContainerCreates] {
					t.Fatal("package recovery charged publication or execution twice")
				}
			}
			wantState := domain.RunNeedsReview
			if mode == "pass" {
				wantState = domain.RunReady
			}
			if err != nil || result.State != wantState || result.ActiveStartedAt != nil || result.WorkflowRevision != workflow.GenerationRevision {
				t.Fatalf("forward data run: %+v %v", result, err)
			}
			input, err := data.Reader().ReadInput(ctx, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			wantStage, wantCalls := domain.StageName("solution_decision"), int32(3)
			if mode != "wrong_answer" {
				wantStage, wantCalls = "judge", 4
				if mode == "pass" || mode == "differential_wa" || mode == "reference_tle" {
					wantStage = "quality"
				}
				if mode == "pass" {
					wantStage = "package"
				}
				if input.Value == nil {
					t.Fatalf("passing Solution did not admit data: %+v", input)
				}
				content, err := data.Reader().ReadDraft(ctx, result.RunID)
				if err != nil || content.ValidateInput(*input.Value) != nil || len(content.Plan.Cases) != 4 || content.Plan.EffectiveSeed != seed {
					t.Fatalf("committed data: %+v %v", content, err)
				}
				stored, err := f.store.ReadCommittedLLMStage(ctx, result.RunID, "data")
				if err != nil || stored.Attempt.OutputDigest == nil || *stored.Attempt.OutputDigest != content.ContentDigest {
					t.Fatalf("data has no atomic stage proof: %v", err)
				}
				raw, err := json.Marshal(content)
				if err != nil {
					t.Fatal(err)
				}
				reloaded, err := data.Reader().ReadDraft(ctx, result.RunID)
				if err != nil {
					t.Fatal(err)
				}
				again, _ := json.Marshal(reloaded)
				if string(raw) != string(again) {
					t.Fatal("reconstructed plan changed source bytes or seeds")
				}
				report, err := data.Reader().ReadVerification(ctx, result.RunID)
				if err != nil || report.Passed != (mode == "pass" || mode == "differential_wa" || mode == "reference_tle") {
					t.Fatalf("actual data verification: %+v %v", report, err)
				}
				manifest, err := report.Dataset(*input.Value, content)
				if report.Passed {
					if err != nil || len(manifest.Inputs) != 6 {
						t.Fatalf("validated dataset: %+v %v", manifest, err)
					}
					assertDataCommittedReadsRejectSubstitution(t, ctx, f, generationConfig, similarityConfig, base, result.RunID)
					judgeInput, err := data.Reader().ReadJudgeInput(ctx, result.RunID)
					if err != nil {
						t.Fatal(err)
					}
					judgeReport, err := data.Reader().ReadJudgeVerification(ctx, result.RunID)
					if err != nil || judgeReport.ValidateFor(judgeInput) != nil || judgeReport.Passed != (mode == "pass") {
						t.Fatalf("actual Judge report: %+v %v", judgeReport, err)
					}
					answers, err := judgeReport.Dataset(judgeInput)
					if mode == "pass" {
						if err != nil || len(answers.Cases) != 6 {
							t.Fatalf("Judge answers: %+v %v", answers, err)
						}
						assertJudgeCommittedReadsRejectSubstitution(t, ctx, f, generationConfig, similarityConfig, base, result.RunID)
						qualityInput, err := quality.Reader().ReadInput(ctx, result.RunID)
						if err != nil {
							t.Fatal(err)
						}
						qualityReport, err := quality.Reader().ReadReport(ctx, result.RunID)
						if err != nil || !qualityReport.Passed || qualityReport.ValidateFor(result.RunID, qualityInput) != nil || len(qualityReport.Canaries) != 2 || len(qualityReport.Cases) != 6 {
							t.Fatalf("current quality report: %+v %v", qualityReport, err)
						}
						packages, err := application.NewPackageExecutor(quality)
						if err != nil {
							t.Fatal(err)
						}
						raw, record, err := packages.Reader().ReadArchive(ctx, result.RunID)
						if err != nil || result.FinalPackageOccurrenceID == nil || record.OccurrenceID != *result.FinalPackageOccurrenceID {
							t.Fatalf("verified package read: %+v %v", record, err)
						}
						verified, err := packageprobe.ReadArchive(ctx, raw)
						if err != nil || verified.Manifest.PackageID != record.Binding.PackageID || len(verified.Manifest.TestGroups) != 1 || len(verified.Manifest.TestGroups[0].Tests) != 6 {
							t.Fatalf("archive lost verified dataset: %v", err)
						}
						assertQualityCommittedReadsRejectSubstitution(t, ctx, f, generationConfig, similarityConfig, base, result.RunID)
					} else {
						wantedReason := map[string]string{"differential_wa": "generated/002.in:differential.WA", "reference_tle": "generated/003.in:reference.TLE"}[mode]
						if err == nil || judgeReport.Reason != wantedReason {
							t.Fatalf("Judge failure published answers or lost cause: %+v %v", judgeReport, err)
						}
					}
				} else if err == nil {
					t.Fatal("invalid or non-reproducible data was promoted to a dataset")
				}
				budget, err := f.store.BudgetSnapshot(ctx, result.RunID)
				wantCreates := map[string]int64{"pass": 96, "invalid_generated": 34, "nondeterministic": 32, "differential_wa": 64, "reference_tle": 66}[mode]
				if err != nil || budget.Remaining[domain.BudgetDockerContainerCreates] != 100-wantCreates {
					t.Fatalf("data reproduction did not consume independent execution calls: %+v %v", budget, err)
				}
			} else {
				if input.Review == nil {
					t.Fatal("failed Solution admitted data")
				}
				if _, err := data.Reader().ReadDraft(ctx, result.RunID); err == nil {
					t.Fatal("data draft exists after failed Solution")
				}
			}
			if result.CurrentStage != wantStage || f.httpCalls.Load() != wantCalls || f.sends.Load() != 1 {
				t.Fatalf("unexpected forward route: %+v LLM=%d similarity=%d", result, f.httpCalls.Load(), f.sends.Load())
			}
			resumed, err := newService().Resume(ctx, result.RunID)
			if err != nil || resumed.Version != result.Version || f.httpCalls.Load() != wantCalls || f.sends.Load() != 1 {
				t.Fatalf("review resumed unrequested work: %+v %v", resumed, err)
			}
		})
	}
}

var errDataReportGap = errors.New("injected data report publication gap")
var errJudgeReportGap = errors.New("injected Judge report publication gap")
var errQualityReportGap = errors.New("injected quality report publication gap")
var errPackageCommitGap = errors.New("injected package commit gap")

type dataReportGapStore struct {
	*sqlite.Store
	fired        atomic.Bool
	judgeFired   atomic.Bool
	qualityFired atomic.Bool
	packageFired atomic.Bool
}

func (s *dataReportGapStore) FinalizeVerifiedPackage(ctx context.Context, command domain.FinalizeVerifiedPackageCommand) (domain.RunSnapshot, error) {
	if s.packageFired.CompareAndSwap(false, true) {
		return domain.RunSnapshot{}, errPackageCommitGap
	}
	return s.Store.FinalizeVerifiedPackage(ctx, command)
}

func (s *dataReportGapStore) CreateArtifactDeclaration(ctx context.Context, declaration domain.ArtifactDeclarationRecord) error {
	if declaration.StageName == "data_verify" && declaration.LogicalPath == "data/verification.json" && s.fired.CompareAndSwap(false, true) {
		return errDataReportGap
	}
	if declaration.StageName == "judge" && declaration.LogicalPath == "judge/verification.json" && s.judgeFired.CompareAndSwap(false, true) {
		return errJudgeReportGap
	}
	if declaration.StageName == "quality" && declaration.LogicalPath == "quality/report.json" && s.qualityFired.CompareAndSwap(false, true) {
		return errQualityReportGap
	}
	return s.Store.CreateArtifactDeclaration(ctx, declaration)
}

func assertDataCommittedReadsRejectSubstitution(t *testing.T, ctx context.Context, f *similarityExecutorFixture, generationConfig application.GenerationExecutorConfig, similarityConfig application.SimilarityExecutorConfig, base application.DockerSandboxConfig, runID domain.RunID) {
	t.Helper()
	for _, path := range []domain.SafeRelPath{"data/plan.json", "data/dataset.json", "data/generated/001.in", "data/generator/main.cpp"} {
		changed := generationConfig
		changed.Store = &alteredSolutionStageStore{f.store, func(stage *port.CommittedPrivateStage) {
			if stage.Attempt.StageName != "data_verify" {
				return
			}
			for i, item := range stage.Artifacts {
				if item.Blob.LogicalPath == path {
					stage.Artifacts = append(stage.Artifacts[:i], stage.Artifacts[i+1:]...)
					return
				}
			}
		}}
		generation, err := application.NewGenerationExecutor(changed)
		if err != nil {
			t.Fatal(err)
		}
		similarityConfig.Generation = generation
		similarity, err := application.NewSimilarityExecutor(similarityConfig)
		if err != nil {
			t.Fatal(err)
		}
		solution, err := application.NewSolutionExecutor(similarity, generation)
		if err != nil {
			t.Fatal(err)
		}
		data, err := application.NewDataExecutor(solution, base)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := data.Reader().ReadVerification(ctx, runID); err == nil {
			t.Fatalf("missing committed data artifact accepted: %s", path)
		}
	}
}
