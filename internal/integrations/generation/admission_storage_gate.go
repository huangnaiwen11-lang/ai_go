package generation

import (
	"errors"
	"fmt"
	"os"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/r2"
)

// ErrAdmissionStorageConfig means B2B new-task admission would be able to
// reserve and submit a job without an owned-result storage path. It is a
// startup/configuration failure, never a per-request fallback to local.
var ErrAdmissionStorageConfig = errors.New("generation admission result storage configuration unavailable")

// B2BAdmissionStorageReadiness is intentionally only a validated config
// snapshot. It does not build an S3 client or contact R2, so installing the
// gate into an HTTP composition root cannot perform external I/O.
type B2BAdmissionStorageReadiness struct {
	b2bEnabled bool
	storage    r2.Config
}

// NewB2BAdmissionStorageReadiness reads R2 settings only when the new-task
// selector is B2B. The default local profile therefore keeps its historical
// guarantee: it does not parse, construct or dial R2. A selected B2B provider
// must have the same complete public-R2 config required by the materializer
// Worker before the API can accept a billable creation.
func NewB2BAdmissionStorageReadiness(integrations *conf.Integrations) (B2BAdmissionStorageReadiness, error) {
	if integrations == nil || integrations.GetGeneration() == nil {
		return B2BAdmissionStorageReadiness{}, ErrAdmissionConfig
	}
	switch conf.EffectiveGenerationProvider(integrations.GetGeneration()) {
	case conf.ProviderLocalExecutionV2:
		return B2BAdmissionStorageReadiness{}, nil
	case conf.ProviderPolarStarB2BV2:
		storage, enabled, err := r2.LoadConfig(os.Getenv)
		if err != nil {
			return B2BAdmissionStorageReadiness{}, fmt.Errorf("%w: %w", ErrAdmissionStorageConfig, err)
		}
		if !enabled {
			return B2BAdmissionStorageReadiness{}, ErrAdmissionStorageConfig
		}
		return B2BAdmissionStorageReadiness{b2bEnabled: true, storage: storage}, nil
	default:
		return B2BAdmissionStorageReadiness{}, ErrAdmissionConfig
	}
}

// RequireB2B is a narrow capability check used by the safe admission factory.
// It intentionally does not expose credentials or a mutable storage client.
func (readiness B2BAdmissionStorageReadiness) RequireB2B() error {
	if !readiness.b2bEnabled || readiness.storage.Validate() != nil {
		return ErrAdmissionStorageConfig
	}
	return nil
}
