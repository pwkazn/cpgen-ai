package application_test

import (
	"encoding/json"
	"testing"

	"cpgen/internal/domain"
)

// Regression fixture only: production code contains no graph-specific rules.
// DSU and component relabeling independently implement the input semantics.
func connectivityDockerOutputs(t *testing.T) map[string][]byte {
	t.Helper()
	outputs := llmBuiltinOutputs(t)
	statement := domain.StatementDraftV1{SchemaVersion: domain.StatementDraftSchemaV1, Title: "Online Connectivity Decisions", Description: "Process undirected edges in order. Accept an edge exactly when its endpoints are currently in different connected components, then connect those components. Reject every other edge, including self-loops.", Input: domain.ProblemIO{Description: "Read n and m, then m pairs u v. 1 <= n <= 50000, 0 <= m <= 75000, 1 <= u,v <= n.", Fields: []string{"n and m", "m edges"}}, Output: domain.ProblemIO{Description: "Print m bits in order, 1 for acceptance and 0 for rejection.", Fields: []string{"decision bits"}}, Samples: []domain.ProblemSample{{Input: connectivityRegressionInput, Output: "1101000\n", Explanation: "The fourth edge is accepted."}}}
	solution := domain.SolutionDraftV1{SchemaVersion: domain.SolutionDraftSchemaV1,
		ReferenceCode: `#include <iostream>
#include <vector>
#include <numeric>
int main(){int n,m;if(!(std::cin>>n>>m)||n<1||n>50000||m<0||m>75000)return 3;std::vector<int>p(n+1),sz(n+1,1);std::iota(p.begin(),p.end(),0);auto find=[&](int x){while(x!=p[x]){p[x]=p[p[x]];x=p[x];}return x;};for(int i=0;i<m;i++){int u,v;if(!(std::cin>>u>>v)||u<1||u>n||v<1||v>n)return 3;u=find(u);v=find(v);if(u==v){std::cout<<'0';continue;}std::cout<<'1';if(sz[u]<sz[v])std::swap(u,v);p[v]=u;sz[u]+=sz[v];}std::cout<<std::endl;}
`,
		BruteCode: `#include <iostream>
#include <vector>
int main(){int n,m;if(!(std::cin>>n>>m)||n<1||n>200||m<0||m>1000)return 3;std::vector<int>component(n+1);for(int v=1;v<=n;v++)component[v]=v;for(int i=0;i<m;i++){int u,v;if(!(std::cin>>u>>v)||u<1||u>n||v<1||v>n)return 3;int a=component[u],b=component[v];if(a==b){std::cout<<'0';continue;}std::cout<<'1';for(int w=1;w<=n;w++)if(component[w]==b)component[w]=a;}std::cout<<std::endl;}
`, Explanation: "The reference maintains a disjoint-set forest, joining distinct components by size. Component relabeling is an independent O(n*m) oracle and explicitly rejects n > 200 or m > 1000."}
	var err error
	outputs["statement.draft"], err = json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	outputs["solution.draft"], err = json.Marshal(solution)
	if err != nil {
		t.Fatal(err)
	}
	return outputs
}
