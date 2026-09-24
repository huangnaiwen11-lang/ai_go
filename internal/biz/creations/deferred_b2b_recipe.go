package creations

import (
	"encoding/json"
	"net/url"
	"strings"
)

// DeferredB2BImageToVideo is the immutable public-product half of a two-step
// B2B text-to-video plan.  The first generated image does not exist when the
// user is charged, so Recipe deliberately contains neither source_image nor
// opening_frame.  Only the result materializer may later bind its verified R2
// object as opening_frame before the second submission is frozen.
//
// Digest covers the unbound recipe.  It belongs in the creation fingerprint so
// retrying the same client idempotency key with a different second-stage
// product is a conflict rather than a silent reuse of the first creation.
type DeferredB2BImageToVideo struct {
	Recipe B2BProductRecipe
	Digest string
}

// CompileDeferredB2BImageToVideo freezes the published second-stage product
// before reservation.  It is intentionally narrower than CompileB2BProductRecipe:
// the public recipe must be image_to_video and must leave the opening-frame
// slot empty.  This prevents a client or a published recipe from smuggling an
// arbitrary URL into the second submission before the first owned frame exists.
func CompileDeferredB2BImageToVideo(source PublishedB2BProductRecipe, overrides map[string]json.RawMessage) (*DeferredB2BImageToVideo, error) {
	normalized, err := source.Normalize()
	if err != nil || normalized.Atom != AtomImageToVideo {
		return nil, ErrB2BProductRecipeUnavailable
	}
	recipe, err := normalized.CompileB2BProductRecipe(overrides, nil)
	if err != nil {
		return nil, ErrB2BProductRecipeUnavailable
	}
	deferred := DeferredB2BImageToVideo{Recipe: recipe}
	normalizedDeferred, err := deferred.Normalize()
	if err != nil {
		return nil, err
	}
	return &normalizedDeferred, nil
}

// Normalize verifies the base recipe can only become a two-step image-to-video
// submission after an owned opening frame is inserted.  It copies all bytes so
// a template-reader cache cannot mutate an already accepted creation.
func (deferred DeferredB2BImageToVideo) Normalize() (DeferredB2BImageToVideo, error) {
	recipe, err := deferred.Recipe.Normalize()
	if err != nil || recipeContainsOpeningFrameBinding(recipe) {
		return DeferredB2BImageToVideo{}, ErrB2BProductRecipeUnavailable
	}
	digest, err := recipe.Digest()
	if err != nil || (deferred.Digest != "" && deferred.Digest != digest) {
		return DeferredB2BImageToVideo{}, ErrB2BProductRecipeUnavailable
	}
	return DeferredB2BImageToVideo{Recipe: recipe, Digest: digest}, nil
}

// BindOwnedOpeningFrame derives the final public product for the second step.
// The URL is restricted to canonical HTTPS object syntax here; the caller is
// additionally responsible for proving it was written to our immutable R2
// object before this method is called.  It does not mutate the deferred base,
// so durable pending->consumed CAS remains the one-shot authorization point.
func (deferred DeferredB2BImageToVideo) BindOwnedOpeningFrame(storageURL string) (B2BProductRecipe, error) {
	normalized, err := deferred.Normalize()
	if err != nil || !validDeferredOwnedOpeningFrameURL(storageURL) {
		return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	recipe := normalized.Recipe
	recipe.Assets = append(append([]B2BAsset(nil), recipe.Assets...), B2BAsset{Role: "opening_frame", URL: storageURL})
	bound, err := recipe.Normalize()
	if err != nil {
		return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	return bound, nil
}

func recipeContainsOpeningFrameBinding(recipe B2BProductRecipe) bool {
	for _, asset := range recipe.Assets {
		if asset.Role == "source_image" || asset.Role == "opening_frame" {
			return true
		}
	}
	var fields map[string]json.RawMessage
	return json.Unmarshal(recipe.Input, &fields) != nil || fields == nil || fields["imageUrl"] != nil
}

func validDeferredOwnedOpeningFrameURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw == strings.TrimSpace(raw) && parsed.Scheme == "https" && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path != "" && parsed.String() == raw
}
