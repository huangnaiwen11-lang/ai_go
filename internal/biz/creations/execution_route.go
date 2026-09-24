package creations

import (
	"errors"
	"regexp"
	"strings"
)

// ExecutionRoute is the immutable provider identity frozen on a creation step.
// It deliberately contains no credentials; credential rotation must not alter a
// step's execution identity.
type ExecutionRoute struct {
	Provider        string
	AccountRef      string
	ContractVersion string
	MappingVersion  string
}

const (
	LocalExecutionProvider = "local_execution_v2"
	PolarStarB2BProvider   = "polarstar_b2b_v2"
	DefaultLocalAccount    = "local-default"
	LocalContractVersion   = "execution.v2"
	LocalMappingVersion    = "local.v1"
	B2BContractVersion     = "b2b.job.v2"
)

var executionAccountRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// NormalizeExecutionRoute validates a frozen route and expands an all-empty
// route from historical documents to the only compatible local route. A
// partially populated route is rejected instead of inferring the current
// configuration or silently crossing provider protocols.
func NormalizeExecutionRoute(route ExecutionRoute) (ExecutionRoute, error) {
	if route == (ExecutionRoute{}) {
		return LocalExecutionRoute(), nil
	}
	if strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.AccountRef) == "" || strings.TrimSpace(route.ContractVersion) == "" || strings.TrimSpace(route.MappingVersion) == "" {
		return ExecutionRoute{}, errors.New("execution route must contain provider, account_ref, contract_version, and mapping_version")
	}
	if route.Provider == LocalExecutionProvider {
		if route.AccountRef != DefaultLocalAccount || route.ContractVersion != LocalContractVersion || route.MappingVersion != LocalMappingVersion {
			return ExecutionRoute{}, errors.New("local execution route has incompatible account or version")
		}
		return route, nil
	}
	if route.Provider != PolarStarB2BProvider {
		return ExecutionRoute{}, errors.New("unknown execution provider")
	}
	if !executionAccountRefPattern.MatchString(route.AccountRef) || len(route.AccountRef) > 200 {
		return ExecutionRoute{}, errors.New("invalid execution account_ref")
	}
	if route.AccountRef == DefaultLocalAccount {
		return ExecutionRoute{}, errors.New("PolarStar B2B route cannot use the local account")
	}
	if route.ContractVersion != B2BContractVersion || !validRouteVersion(route.MappingVersion) {
		return ExecutionRoute{}, errors.New("invalid PolarStar B2B contract or mapping version")
	}
	return route, nil
}

func LocalExecutionRoute() ExecutionRoute {
	return ExecutionRoute{Provider: LocalExecutionProvider, AccountRef: DefaultLocalAccount, ContractVersion: LocalContractVersion, MappingVersion: LocalMappingVersion}
}

func validRouteVersion(value string) bool {
	return len(value) <= 200 && executionAccountRefPattern.MatchString(value)
}
