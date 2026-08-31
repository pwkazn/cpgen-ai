//go:build cpgen_slice0_probe

package ab

import (
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const FixedSeed uint64 = 0x5eed1234

//go:embed reference.cpp
var referenceSource []byte

//go:embed brute.cpp
var bruteSource []byte

//go:embed validator.cpp
var validatorSource []byte

//go:embed checker.cpp
var checkerSource []byte

//go:embed generator.cpp
var generatorSource []byte

type Definition struct {
	Name   string
	Role   port.ProgramRole
	Source []byte
}

type BlobInserter interface {
	PutBlob([]byte) domain.BlobRef
}

func Definitions() []Definition {
	definitions := []Definition{
		{Name: "brute", Role: port.RoleBrute, Source: bruteSource},
		{Name: "checker", Role: port.RoleChecker, Source: checkerSource},
		{Name: "generator", Role: port.RoleGenerator, Source: generatorSource},
		{Name: "reference", Role: port.RoleSolution, Source: referenceSource},
		{Name: "validator", Role: port.RoleValidator, Source: validatorSource},
	}
	for index := range definitions {
		definitions[index].Source = slices.Clone(definitions[index].Source)
	}
	return definitions
}

func Bundle(store BlobInserter, name string) (port.SourceBundleManifest, error) {
	if store == nil {
		return port.SourceBundleManifest{}, fmt.Errorf("A+B fixture blob inserter is required")
	}
	var source []byte
	for _, definition := range Definitions() {
		if definition.Name == name {
			source = definition.Source
			break
		}
	}
	if source == nil {
		return port.SourceBundleManifest{}, fmt.Errorf("unknown A+B fixture %q", name)
	}
	manifest := port.SourceBundleManifest{
		SchemaVersion: domain.DomainSchemaVersion,
		Files:         []port.SourceFile{{Path: "main.cpp", Blob: store.PutBlob(source)}},
		EntryPoint:    "main.cpp",
	}
	digest, err := port.ComputeSourceBundleDigest(manifest)
	if err != nil {
		return port.SourceBundleManifest{}, err
	}
	manifest.Digest = digest
	return manifest, nil
}

type Case struct {
	A int64
	B int64
}

func GeneratedCases(seed uint64) []Case {
	result := []Case{
		{A: -1_000_000_000, B: -1_000_000_000},
		{A: 1_000_000_000, B: 1_000_000_000},
		{A: -1_000_000_000, B: 1_000_000_000},
		{A: 0, B: 0},
	}
	state := seed
	for range 2 {
		result = append(result, Case{A: bounded(&state), B: bounded(&state)})
	}
	return result
}

func RenderCases(cases []Case) string {
	var output strings.Builder
	for _, test := range cases {
		output.WriteString(strconv.FormatInt(test.A, 10))
		output.WriteByte(' ')
		output.WriteString(strconv.FormatInt(test.B, 10))
		output.WriteByte('\n')
	}
	return output.String()
}

func nextValue(state *uint64) uint64 {
	*state += 0x9e3779b97f4a7c15
	value := *state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func bounded(state *uint64) int64 {
	return int64(nextValue(state)%2_000_000_001) - 1_000_000_000
}
