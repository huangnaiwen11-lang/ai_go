package data

import (
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/data/model"
)

func TestCreationStepRouteRoundTripFlattensPO(t *testing.T) {
	want := creations.ExecutionRoute{Provider: creations.PolarStarB2BProvider, AccountRef: "acct-main", ContractVersion: creations.B2BContractVersion, MappingVersion: "polarstar.image.v1"}
	document, err := newCreationStepDocument(creations.CreationStep{ID: "step-1", CreationID: "creation-1", Route: want})
	if err != nil {
		t.Fatalf("newCreationStepDocument() error = %v", err)
	}
	if document.Provider != want.Provider || document.AccountRef != want.AccountRef || document.ContractVersion != want.ContractVersion || document.MappingVersion != want.MappingVersion {
		t.Fatalf("document route = %#v, want flattened route %#v", document, want)
	}
	got, err := toBizCreationStep(document)
	if err != nil {
		t.Fatalf("toBizCreationStep() error = %v", err)
	}
	if got.Route != want {
		t.Fatalf("round-trip route = %#v, want %#v", got.Route, want)
	}
}

func TestCreationStepRouteMissingPOFieldsUsesHistoricalLocalRoute(t *testing.T) {
	got, err := toBizCreationStep(model.CreationStepDocument{ID: "legacy-step"})
	if err != nil {
		t.Fatalf("toBizCreationStep() error = %v", err)
	}
	if got.Route != creations.LocalExecutionRoute() {
		t.Fatalf("legacy route = %#v, want local compatibility route", got.Route)
	}
}

func TestCreationStepRouteRejectsInvalidPOFields(t *testing.T) {
	_, err := toBizCreationStep(model.CreationStepDocument{ID: "bad-step", Provider: creations.PolarStarB2BProvider, AccountRef: "acct-main", ContractVersion: "wrong", MappingVersion: "map.v1"})
	if err == nil {
		t.Fatal("toBizCreationStep() accepted invalid frozen route")
	}
}
