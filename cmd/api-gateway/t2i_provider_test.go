package main

import (
	"testing"

	"ai-business-service/internal/conf"
)

func TestUsesB2BProductRecipesOnlyForB2BNewTaskSelector(t *testing.T) {
	cases := map[string]struct {
		integrations *conf.Integrations
		want         bool
	}{
		"missing config": {nil, false},
		"implicit local": {&conf.Integrations{Generation: &conf.Integrations_Generation{}}, false},
		"explicit local": {&conf.Integrations{Generation: &conf.Integrations_Generation{Provider: conf.ProviderLocalExecutionV2}}, false},
		"b2b":            {&conf.Integrations{Generation: &conf.Integrations_Generation{Provider: conf.ProviderPolarStarB2BV2}}, true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := usesB2BProductRecipes(testCase.integrations); got != testCase.want {
				t.Fatalf("usesB2BProductRecipes() = %t, want %t", got, testCase.want)
			}
		})
	}
}
