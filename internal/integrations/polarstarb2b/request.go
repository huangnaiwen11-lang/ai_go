package polarstarb2b

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrInvalidRequest = errors.New("polarstar b2b: invalid request")

// RestoreRequest validates a persisted immutable request without remapping it
// against today's catalog. Digest is integrity, not authentication: callers must
// obtain route/digest/bytes from the same trusted persisted submission intent.
func RestoreRequest(route Route, payload []byte, digest string) (Request, error) {
	if !validRoute(route) || len(payload) == 0 || len(payload) > 128<<10 {
		return Request{}, ErrInvalidRequest
	}
	payload = append([]byte(nil), payload...)
	sum := sha256.Sum256(payload)
	if digest != hex.EncodeToString(sum[:]) || validateJSON(payload) != nil {
		return Request{}, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	var wire requestWire
	if json.Unmarshal(payload, &fields) != nil || json.Unmarshal(payload, &wire) != nil {
		return Request{}, ErrInvalidRequest
	}
	for _, name := range []string{"externalId", "idempotencyKey", "capability", "model", "input", "callbackUrl", "callbackPolicy", "resultUrlPolicy"} {
		if _, ok := fields[name]; !ok {
			return Request{}, ErrInvalidRequest
		}
	}
	for name := range fields {
		switch name {
		case "externalId", "idempotencyKey", "capability", "model", "input", "callbackUrl", "callbackPolicy", "resultUrlPolicy", "templateId":
		default:
			return Request{}, ErrInvalidRequest
		}
	}
	if wire.ExternalID != route.StepID || wire.IdempotencyKey != "cling-step:"+route.StepID || !supportedCapability(wire.Capability) || !publicModelID.MatchString(wire.Model) || len(wire.Model) > 100 || (wire.TemplateID != "" && !validID(wire.TemplateID, 200)) || wire.ResultURLPolicy != "permanent" {
		return Request{}, ErrInvalidRequest
	}
	switch wire.CallbackPolicy {
	case "disabled":
		if wire.CallbackURL != nil {
			return Request{}, ErrInvalidRequest
		}
	case "bounded":
		if wire.CallbackURL == nil {
			return Request{}, ErrInvalidRequest
		}
		if _, ok := publicHTTPSURL(*wire.CallbackURL); !ok {
			return Request{}, ErrInvalidRequest
		}
	default:
		return Request{}, ErrInvalidRequest
	}
	var inputFields map[string]json.RawMessage
	if json.Unmarshal(fields["input"], &inputFields) != nil || inputFields == nil {
		return Request{}, ErrInvalidRequest
	}
	allowed := make([]string, 0, len(inputFields))
	for field, raw := range inputFields {
		if !capabilityInput(wire.Capability, field) || string(raw) == "null" {
			return Request{}, ErrInvalidRequest
		}
		allowed = append(allowed, field)
	}
	// Published SKU dimensions/ratios were approved before persistence. Validate
	// public capability semantics again, without silently using a newer mapping.
	m := ModelMapping{Capability: wire.Capability, Model: wire.Model, AllowedInputs: allowed, Sizes: []ImageSize{{wire.Input.Width, wire.Input.Height}}, AspectRatios: []string{wire.Input.AspectRatio}, Durations: []int{5, 10, 15}}
	if !validateInput(wire.Input, m, wire.TemplateID, false) {
		return Request{}, ErrInvalidRequest
	}
	return Request{payload: payload, digest: digest, externalID: route.StepID, capability: wire.Capability, route: route}, nil
}

// Route is selected and persisted upstream before submission; it never contains credentials.
type Route struct{ StepID, Provider, AccountRef, ContractVersion, MappingVersion string }

// Input contains only public B2B controls. Private execution parameters have no representation.
type Input struct {
	Prompt              string   `json:"prompt,omitempty"`
	NegativePrompt      string   `json:"negativePrompt,omitempty"`
	ImageURL            string   `json:"imageUrl,omitempty"`
	FaceImageURL        string   `json:"faceImageUrl,omitempty"`
	GarmentImageURL     string   `json:"garmentImageUrl,omitempty"`
	GuideImageURL       string   `json:"guideImageUrl,omitempty"`
	AdditionalImageURLs []string `json:"additionalImageUrls,omitempty"`
	ReferenceImageURLs  []string `json:"referenceImageUrls,omitempty"`
	DurationSeconds     int      `json:"durationSeconds,omitempty"`
	AspectRatio         string   `json:"aspectRatio,omitempty"`
	EnableAudio         *bool    `json:"enableAudio,omitempty"`
	AudioPrompt         string   `json:"audioPrompt,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
	Width               int      `json:"width,omitempty"`
	Height              int      `json:"height,omitempty"`
}

type ProductInput struct {
	Capability, ModelKey, TemplateKey string
	Input                             Input
	Assets                            []Asset
}

// Asset is an upstream-authorized image with a product role, never a private workflow input.
type Asset struct{ Role, URL string }
type CallbackConfig struct{ Mode, URL string }
type ImageSize struct{ Width, Height int }
type ModelMapping struct {
	Capability, Model string
	AllowedInputs     []string
	AspectRatios      []string
	Sizes             []ImageSize
	Durations         []int
	Templates         map[string]string
}

// MappingSnapshot must come from an approved, published version; this adapter never fetches a catalog.
type MappingSnapshot struct {
	Version string
	Models  map[string]ModelMapping
}

// Request owns immutable wire bytes plus their execution scope. It has no API key.
type Request struct {
	payload                        []byte
	digest, externalID, capability string
	route                          Route
}

func (r Request) Payload() []byte        { return append([]byte(nil), r.payload...) }
func (r Request) Digest() string         { return r.digest }
func (r Request) ExternalID() string     { return r.externalID }
func (r Request) IdempotencyKey() string { return "cling-step:" + r.route.StepID }
func (r Request) Capability() string     { return r.capability }
func (r Request) AccountRef() string     { return r.route.AccountRef }
func (r Request) Route() Route           { return r.route }
func (r Request) Validate() error {
	if !validRoute(r.route) || len(r.payload) == 0 || r.externalID != r.route.StepID || !supportedCapability(r.capability) {
		return ErrInvalidRequest
	}
	sum := sha256.Sum256(r.payload)
	if r.digest != hex.EncodeToString(sum[:]) {
		return ErrInvalidRequest
	}
	return nil
}
