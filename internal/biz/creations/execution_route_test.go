package creations

import (
	"testing"
	"time"
)

func TestExecutionRouteMappingVersionMustMatchAdapterIdentity(t *testing.T) {
	for _, version := range []string{"map/one", "映射", ".hidden", "map!"} {
		_, err := NormalizeExecutionRoute(ExecutionRoute{Provider: PolarStarB2BProvider, AccountRef: "acct-a", ContractVersion: B2BContractVersion, MappingVersion: version})
		if err == nil {
			t.Errorf("accepted unsupported mapping identity %q", version)
		}
	}
}

func TestNewCreationFactsFreezesLocalRouteForBlockedStep(t *testing.T) {
	request := CreateReservedRequest{Plan: []StepPlan{{Sequence: 1, Atom: AtomTextToImage}, {Sequence: 2, Atom: AtomImageToVideo}}}
	_, steps := newCreationFacts(request, []StepAdmission{{Route: LocalExecutionRoute()}, {Route: LocalExecutionRoute()}}, "fixture", time.Now())
	if len(steps) != 2 {
		t.Fatal("missing steps")
	}
	for _, step := range steps {
		if step.Route != LocalExecutionRoute() {
			t.Fatal("new step route was not frozen")
		}
	}
	if steps[1].SubmitStatus != StepSubmitStatusBlocked {
		t.Fatal("dependency changed")
	}
}

func TestNormalizeExecutionRouteDefaultsOnlyAllEmptyHistoricalRoute(t *testing.T) {
	route, err := NormalizeExecutionRoute(ExecutionRoute{})
	if err != nil {
		t.Fatalf("NormalizeExecutionRoute() error = %v", err)
	}
	want := ExecutionRoute{Provider: LocalExecutionProvider, AccountRef: DefaultLocalAccount, ContractVersion: LocalContractVersion, MappingVersion: LocalMappingVersion}
	if route != want {
		t.Fatalf("route = %#v, want %#v", route, want)
	}
}

func TestNormalizeExecutionRouteAcceptsKnownLocalRoute(t *testing.T) {
	want := ExecutionRoute{Provider: LocalExecutionProvider, AccountRef: DefaultLocalAccount, ContractVersion: LocalContractVersion, MappingVersion: LocalMappingVersion}
	route, err := NormalizeExecutionRoute(want)
	if err != nil || route != want {
		t.Fatalf("NormalizeExecutionRoute() = %#v, %v; want %#v, nil", route, err, want)
	}
}

func TestNormalizeExecutionRouteAcceptsB2BRoute(t *testing.T) {
	want := ExecutionRoute{Provider: PolarStarB2BProvider, AccountRef: "acct-main", ContractVersion: B2BContractVersion, MappingVersion: "polarstar.image.v1"}
	route, err := NormalizeExecutionRoute(want)
	if err != nil || route != want {
		t.Fatalf("NormalizeExecutionRoute() = %#v, %v; want %#v, nil", route, err, want)
	}
}

func TestNormalizeExecutionRouteRejectsPartialOrMixedRoute(t *testing.T) {
	cases := []ExecutionRoute{
		{Provider: LocalExecutionProvider},
		{Provider: PolarStarB2BProvider},
		{Provider: PolarStarB2BProvider, AccountRef: "acct-main", ContractVersion: B2BContractVersion},
		{Provider: LocalExecutionProvider, AccountRef: "acct-main", ContractVersion: LocalContractVersion, MappingVersion: LocalMappingVersion},
		{Provider: PolarStarB2BProvider, AccountRef: DefaultLocalAccount, ContractVersion: B2BContractVersion, MappingVersion: "polarstar.image.v1"},
		{Provider: "unknown", AccountRef: "acct-main", ContractVersion: B2BContractVersion, MappingVersion: "polarstar.image.v1"},
		{Provider: LocalExecutionProvider, AccountRef: DefaultLocalAccount, ContractVersion: B2BContractVersion, MappingVersion: LocalMappingVersion},
	}
	for _, route := range cases {
		route := route
		t.Run(route.Provider+":"+route.AccountRef, func(t *testing.T) {
			if _, err := NormalizeExecutionRoute(route); err == nil {
				t.Fatalf("NormalizeExecutionRoute(%#v) accepted invalid route", route)
			}
		})
	}
}
