package application_test

import (
	"encoding/json"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

// Local model response fixtures; every resulting program executes in Docker.
func dataDockerOutputs(t *testing.T, mode string) map[string][]byte {
	t.Helper()
	outputs := solutionDockerOutputs(t, mode)
	if mode == "differential_wa" || mode == "reference_tle" {
		var solution domain.SolutionDraftV1
		if err := json.Unmarshal(outputs["solution.draft"], &solution); err != nil {
			t.Fatal(err)
		}
		injection := "if(n==6){std::cout<<42;return 0;}"
		if mode == "reference_tle" {
			injection = "if(n==100){for(;;){}}"
		}
		solution.ReferenceCode = strings.Replace(solution.ReferenceCode, "std::vector<std::vector<int>>g(n);", injection+"std::vector<std::vector<int>>g(n);", 1)
		var err error
		outputs["solution.draft"], err = json.Marshal(solution)
		if err != nil {
			t.Fatal(err)
		}
	}
	generator := `#include <iostream>
#include <string>
#include <charconv>
#include <cstdint>
#include <chrono>
int main(int argc,char**argv){
 if(argc!=4)return 3;
 std::string a(argv[1]),b(argv[2]),c(argv[3]);
 if(a.rfind("--seed=",0)||b.rfind("--case=",0)||c.rfind("--kind=",0))return 3;
 std::uint64_t seed=0;int id=0;
 auto x=std::from_chars(a.data()+7,a.data()+a.size(),seed);
 auto y=std::from_chars(b.data()+7,b.data()+b.size(),id);
 if(x.ec!=std::errc()||x.ptr!=a.data()+a.size()||y.ec!=std::errc()||y.ptr!=b.data()+b.size()||id<1||id>4)return 3;
 if(c.substr(7)!=(id<=2?"small":id==3?"boundary":"stress"))return 3;
 if(id==1)std::cout<<"2 1\n1 2\n";
 if(id==2){int n=6,m=1+seed%5;std::cout<<n<<' '<<m<<'\n';for(int i=1;i<=m;i++)std::cout<<i<<' '<<i+1<<'\n';}
 if(id==3)std::cout<<"100 0\n";
 if(id==4){std::cout<<"100 4950\n";for(int i=1;i<=100;i++)for(int j=i+1;j<=100;j++)std::cout<<i<<' '<<j<<'\n';}
 return 0;
}
`
	validator := `#include <iostream>
#include <set>
#include <utility>
#include <algorithm>
int main(){long long n,m;if(!(std::cin>>n>>m)||n<1||n>100||m<0||m>n*(n-1)/2)return 3;
std::set<std::pair<int,int>>edges;
for(int i=0;i<m;i++){int a,b;if(!(std::cin>>a>>b)||a<1||a>n||b<1||b>n||a==b)return 3;if(a>b)std::swap(a,b);if(!edges.insert({a,b}).second)return 3;}
std::cin>>std::ws;return std::cin.peek()==std::char_traits<char>::eof()?0:3;}
`
	if mode == "invalid_generated" {
		generator = "#include <iostream>\nint main(){std::cout<<\"101 0\\n\";}\n"
	}
	if mode == "nondeterministic" {
		generator = strings.Replace(generator, " return 0;", " std::cout<<std::chrono::system_clock::now().time_since_epoch().count()<<'\\n'; return 0;", 1)
	}
	draft := domain.DataDraftV1{SchemaVersion: domain.DataDraftSchemaV1, GeneratorCode: generator, ValidatorCode: validator, Cases: []domain.DataCaseDraft{{Kind: domain.DataCaseSmall, Purpose: "Two connected vertices"}, {Kind: domain.DataCaseSmall, Purpose: "A small seeded partial path"}, {Kind: domain.DataCaseBoundary, Purpose: "Maximum vertex count with no edges"}, {Kind: domain.DataCaseStress, Purpose: "A complete graph at the maximum size"}}}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	outputs["data.draft"] = raw
	return outputs
}
