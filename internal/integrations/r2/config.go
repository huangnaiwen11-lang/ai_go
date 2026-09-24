// Package r2 provides the Cloudflare R2 S3-compatible boundary used by the
// Go application. Its configuration intentionally mirrors the frozen legacy
// contract; it never substitutes a local filesystem or a different S3 vendor.
package r2

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	regionAuto              = "auto"
	defaultPublicBucketName = "ai-host"
	r2EndpointSuffix        = ".r2.cloudflarestorage.com"
)

var (
	// ErrIncompleteConfig means an operator supplied some R2 settings, but not
	// enough to build an authenticated Cloudflare R2 client. Callers must not
	// silently switch to another storage backend in this case.
	ErrIncompleteConfig = errors.New("r2 configuration is incomplete")
	// ErrInvalidConfig means a configured value cannot safely represent the
	// frozen R2 topology. Errors intentionally never include credential values.
	ErrInvalidConfig = errors.New("r2 configuration is invalid")
)

// Config is the process-local Cloudflare R2 configuration. AccessKeyID and
// SecretAccessKey are kept only for the S3 credential provider; callers must
// neither log the value nor expose this object to a transport response.
type Config struct {
	AccountID        string
	AccessKeyID      string
	SecretAccessKey  string
	Region           string
	PublicBucket     string
	PublicURL        string
	PrivateBucket    string
	PrivatePublicURL string
	LegalPageBucket  string
}

// PrivateBucketName returns the explicit private bucket only. It intentionally
// does not fall back to the public bucket: callers serving private media must
// fail closed when the legacy private topology is absent.
func (config Config) PrivateBucketName() string { return config.PrivateBucket }

// LoadConfig reads exactly the legacy environment variable names. It returns
// enabled=false only when no R2 setting is present at all, which keeps the
// Docker local simulator as the default. A partial configuration is an error:
// treating it as disabled would hide an operator mistake until a result has
// already reached a terminal state.
func LoadConfig(getenv func(string) string) (Config, bool, error) {
	if getenv == nil {
		return Config{}, false, ErrInvalidConfig
	}
	values := map[string]string{}
	keys := []string{
		"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY",
		"R2_BUCKET_NAME", "R2_PUBLIC_URL", "R2_PRIVATE_BUCKET_NAME",
		"R2_PRIVATE_PUBLIC_URL", "LEGAL_PAGE_R2_BUCKET_NAME",
	}
	for _, key := range keys {
		values[key] = strings.TrimSpace(getenv(key))
	}
	configured := false
	for _, value := range values {
		if value != "" {
			configured = true
			break
		}
	}
	if !configured {
		return Config{}, false, nil
	}

	config := Config{
		AccountID:        values["R2_ACCOUNT_ID"],
		AccessKeyID:      values["R2_ACCESS_KEY_ID"],
		SecretAccessKey:  values["R2_SECRET_ACCESS_KEY"],
		Region:           regionAuto,
		PublicBucket:     values["R2_BUCKET_NAME"],
		PublicURL:        values["R2_PUBLIC_URL"],
		PrivateBucket:    values["R2_PRIVATE_BUCKET_NAME"],
		PrivatePublicURL: values["R2_PRIVATE_PUBLIC_URL"],
		LegalPageBucket:  values["LEGAL_PAGE_R2_BUCKET_NAME"],
	}
	if config.PublicBucket == "" {
		// This is the legacy backend default. The separate legacy video promotion
		// path used another default; that unresolved inconsistency is not silently
		// selected by this generic client.
		config.PublicBucket = defaultPublicBucketName
	}
	if err := config.Validate(); err != nil {
		return Config{}, false, err
	}
	return config, true, nil
}

// Validate performs only local normalization and shape checks. It does not
// resolve DNS, contact R2, or disclose a credential in an error.
func (config Config) Validate() error {
	if config.Region != regionAuto || !validAccountID(config.AccountID) ||
		!validCredential(config.AccessKeyID) || !validCredential(config.SecretAccessKey) ||
		!validBucket(config.PublicBucket) || !validHTTPSOrigin(config.PublicURL) {
		if config.AccountID == "" || config.AccessKeyID == "" || config.SecretAccessKey == "" || config.PublicURL == "" {
			return ErrIncompleteConfig
		}
		return ErrInvalidConfig
	}
	if config.PrivateBucket != "" && !validBucket(config.PrivateBucket) {
		return ErrInvalidConfig
	}
	if config.PrivatePublicURL != "" && (config.PrivateBucket == "" || !validHTTPSOrigin(config.PrivatePublicURL)) {
		return ErrInvalidConfig
	}
	if config.LegalPageBucket != "" && !validBucket(config.LegalPageBucket) {
		return ErrInvalidConfig
	}
	return nil
}

// Endpoint always derives the Cloudflare R2 S3 endpoint from AccountID. The
// legacy R2_ENDPOINT variable belongs to GPU/onboarding tooling and must not
// override a Go application's storage endpoint.
func (config Config) Endpoint() string {
	return "https://" + config.AccountID + r2EndpointSuffix
}

// PublicObjectURL forms the long-lived public URL only for the public bucket.
// A nonempty public key is required so a caller cannot accidentally publish
// the bucket root as a result URL.
func (config Config) PublicObjectURL(key string) string {
	if config.PublicURL == "" || !validObjectKey(key) {
		return ""
	}
	return config.PublicURL + "/" + key
}

// LegalBucket follows the frozen legal-page policy: an explicit legal bucket
// wins; otherwise the configured private bucket is used. Empty means that the
// caller must fail closed rather than falling back to the public bucket.
func (config Config) LegalBucket() string {
	if config.LegalPageBucket != "" {
		return config.LegalPageBucket
	}
	return config.PrivateBucket
}

func validAccountID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func validCredential(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func validBucket(value string) bool {
	if value == "" || len(value) > 255 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func validHTTPSOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != raw {
		return false
	}
	return true
}

func validObjectKey(key string) bool {
	return key != "" && len(key) <= 1024 && strings.TrimSpace(key) == key && !strings.HasPrefix(key, "/")
}
