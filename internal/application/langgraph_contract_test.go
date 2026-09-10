package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/smallnest/langgraphgo/graph"
)

// These are pinned-library compatibility probes for INT-03, not a production
// workflow or evidence of durable graph acceptance. The application bridge
// must supply its own commit checks, compatible resume state and node guards.
type graphContractState struct {
	Candidate string
	Committed string
	Review    bool
}

func TestLangGraphContractChecksCommitBeforeAdvancingWithoutHiddenRetry(t *testing.T) {
	commitFailure := errors.New("fixture commit failure")
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "commit-failure"}[fail], func(t *testing.T) {
			var calls []string
			g := graph.NewStateGraph[graphContractState]()
			g.AddNode("idea", "typed candidate and explicit commit", func(ctx context.Context, state graphContractState) (graphContractState, error) {
				if err := ctx.Err(); err != nil {
					return state, err
				}
				calls = append(calls, "idea")
				state.Candidate = "candidate"
				if fail {
					return state, commitFailure
				}
				state.Committed = state.Candidate
				return state, nil
			})
			g.AddNode("statement", "consume committed typed result", func(ctx context.Context, state graphContractState) (graphContractState, error) {
				calls = append(calls, "statement")
				if state.Committed != "candidate" {
					return state, errors.New("advanced without commit")
				}
				return state, nil
			})
			g.SetEntryPoint("idea")
			g.AddEdge("idea", "statement")
			g.AddEdge("statement", graph.END)
			runnable, err := g.Compile()
			if err != nil {
				t.Fatal(err)
			}
			_, err = runnable.Invoke(context.Background(), graphContractState{})
			if fail {
				if !errors.Is(err, commitFailure) || !reflect.DeepEqual(calls, []string{"idea"}) {
					t.Fatalf("calls=%v error=%v", calls, err)
				}
			} else if err != nil || !reflect.DeepEqual(calls, []string{"idea", "statement"}) {
				t.Fatalf("calls=%v error=%v", calls, err)
			}
		})
	}
}

func TestLangGraphContractRoutesTypedReviewAndHonorsCancellation(t *testing.T) {
	for _, review := range []bool{false, true} {
		g := graph.NewStateGraph[graphContractState]()
		g.AddNode("similarity", "typed decision", func(ctx context.Context, state graphContractState) (graphContractState, error) {
			return state, ctx.Err()
		})
		var continued bool
		g.AddNode("continue", "implemented slice boundary", func(ctx context.Context, state graphContractState) (graphContractState, error) {
			if err := ctx.Err(); err != nil {
				return state, err
			}
			continued = true
			return state, nil
		})
		g.SetEntryPoint("similarity")
		g.AddConditionalEdge("similarity", func(_ context.Context, state graphContractState) string {
			if state.Review {
				return graph.END
			}
			return "continue"
		})
		g.AddEdge("continue", graph.END)
		runnable, err := g.Compile()
		if err != nil {
			t.Fatal(err)
		}
		state, err := runnable.Invoke(context.Background(), graphContractState{Review: review})
		if err != nil || state.Review != review || continued == review {
			t.Fatalf("review=%v continued=%v state=%#v err=%v", review, continued, state, err)
		}
		continued = false
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = runnable.Invoke(ctx, graphContractState{})
		if !errors.Is(err, context.Canceled) || continued {
			t.Fatalf("continued=%v error=%v", continued, err)
		}
	}
}
