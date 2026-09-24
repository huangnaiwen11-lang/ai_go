package conf

import "strings"

// AppInstanceEnvironment is the non-secret identity input consumed by the
// publication preflight command. MongoURI is retained only for local checks
// and must not be emitted by command reports or errors.
type AppInstanceEnvironment struct {
	AppID               string
	AppScopeEnabled     bool
	MongoProfile        string
	PublicOrigin        string
	MongoURI            string
	MongoDatabase       string
	R2PublicBucket      string
	R2PrivateBucket     string
	PolarStarAccountRef string
	PolarStarTenantID   string
	CallbackOrigin      string
}

// LoadAppInstanceEnvironment reads the existing integration variable names
// plus the explicit Cling application identity variables. Empty values remain
// empty so callers can reject incomplete publication inputs.
func LoadAppInstanceEnvironment(getenv func(string) string) AppInstanceEnvironment {
	if getenv == nil {
		return AppInstanceEnvironment{}
	}
	return AppInstanceEnvironment{
		AppID:               strings.TrimSpace(getenv("CLING_APP_ID")),
		AppScopeEnabled:     strings.EqualFold(strings.TrimSpace(getenv("GATEWAY_APP_SCOPE_ENABLED")), "true"),
		MongoProfile:        strings.TrimSpace(getenv("CLING_MONGO_PROFILE")),
		PublicOrigin:        strings.TrimSpace(getenv("CLING_PUBLIC_ORIGIN")),
		MongoURI:            strings.TrimSpace(getenv("CLING_MONGO_URI")),
		MongoDatabase:       strings.TrimSpace(getenv("CLING_MONGO_DATABASE")),
		R2PublicBucket:      strings.TrimSpace(getenv("R2_BUCKET_NAME")),
		R2PrivateBucket:     strings.TrimSpace(getenv("R2_PRIVATE_BUCKET_NAME")),
		PolarStarAccountRef: strings.TrimSpace(getenv("POLARSTAR_B2B_ACCOUNT_REF")),
		PolarStarTenantID:   strings.TrimSpace(getenv("POLARSTAR_B2B_TENANT_ID")),
		CallbackOrigin:      strings.TrimSpace(getenv("POLARSTAR_B2B_CALLBACK_ORIGIN")),
	}
}
