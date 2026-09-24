package creations

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCompileDeferredB2BImageToVideo冻结基础配方且只允许受控首帧绑定(t *testing.T) {
	source := PublishedB2BProductRecipe{
		TemplateID: "video-template", TemplateVersion: 7, Atom: AtomImageToVideo,
		ProductKey: "video-standard", TemplateKey: "video-template-v7",
		Input:             json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":10}`),
		AllowedUserInputs: []string{"durationSeconds"},
	}

	deferred, err := CompileDeferredB2BImageToVideo(source, map[string]json.RawMessage{
		"durationSeconds": json.RawMessage(`15`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Digest == "" || len(deferred.Recipe.Assets) != 0 || string(deferred.Recipe.Input) != `{"durationSeconds":15,"prompt":"cinematic motion"}` {
		t.Fatalf("deferred = %#v", deferred)
	}

	bound, err := deferred.BindOwnedOpeningFrame("https://pub.example.test/gen/text_to_image/tenant/frame.png")
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.Assets) != 1 || bound.Assets[0] != (B2BAsset{Role: "opening_frame", URL: "https://pub.example.test/gen/text_to_image/tenant/frame.png"}) {
		t.Fatalf("bound assets = %#v", bound.Assets)
	}
	if string(bound.Input) != string(deferred.Recipe.Input) || bound.ProductKey != deferred.Recipe.ProductKey || bound.TemplateKey != deferred.Recipe.TemplateKey {
		t.Fatalf("binding drifted frozen recipe: deferred=%#v bound=%#v", deferred, bound)
	}
	if _, err := bound.Digest(); err != nil {
		t.Fatalf("bound recipe is not canonical: %v", err)
	}
}

func TestCompileDeferredB2BImageToVideo拒绝预先绑定或绕过首帧的配方(t *testing.T) {
	base := PublishedB2BProductRecipe{
		TemplateID: "video-template", TemplateVersion: 7, Atom: AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":10}`),
	}
	cases := []struct {
		name   string
		mutate func(*PublishedB2BProductRecipe)
	}{
		{
			name: "source image asset",
			mutate: func(recipe *PublishedB2BProductRecipe) {
				recipe.Assets = []B2BAsset{{Role: "source_image", URL: "https://assets.example.test/user.png"}}
			},
		},
		{
			name: "opening frame asset",
			mutate: func(recipe *PublishedB2BProductRecipe) {
				recipe.Assets = []B2BAsset{{Role: "opening_frame", URL: "https://assets.example.test/old-frame.png"}}
			},
		},
		{
			name: "raw image url input",
			mutate: func(recipe *PublishedB2BProductRecipe) {
				recipe.Input = json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":10,"imageUrl":"https://assets.example.test/user.png"}`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recipe := base
			tc.mutate(&recipe)
			if _, err := CompileDeferredB2BImageToVideo(recipe, nil); !errors.Is(err, ErrB2BProductRecipeUnavailable) {
				t.Fatalf("CompileDeferredB2BImageToVideo() error = %v, want ErrB2BProductRecipeUnavailable", err)
			}
		})
	}
}

func TestDeferredB2BImageToVideo拒绝非自有形态的首帧URL(t *testing.T) {
	deferred, err := CompileDeferredB2BImageToVideo(PublishedB2BProductRecipe{
		TemplateID: "video-template", TemplateVersion: 7, Atom: AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":10}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"", "http://pub.example.test/frame.png", "https://pub.example.test/frame.png?provider=signed", "https://user@pub.example.test/frame.png", "https://pub.example.test/frame.png#fragment",
	} {
		if _, err := deferred.BindOwnedOpeningFrame(raw); !errors.Is(err, ErrB2BProductRecipeUnavailable) {
			t.Fatalf("BindOwnedOpeningFrame(%q) error = %v, want ErrB2BProductRecipeUnavailable", raw, err)
		}
	}
}
