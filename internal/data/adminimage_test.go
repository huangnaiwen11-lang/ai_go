package data

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAdminImageMapsLegacyImagePoolsAndInputImages(t *testing.T) {
	created := time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC)
	generated := generatedImage(bson.M{
		"_id":         bson.NewObjectID(),
		"createdAt":   created,
		"provider":    "comfyui-qwen-edit",
		"inputImages": bson.A{"", nil, bson.M{"object": true}, "image-url"},
	})
	if generated.ImagePool != "qwen-edit-default" || generated.AdditionalImageCount != 2 {
		t.Fatalf("generated = %#v", generated)
	}

	faceSwap := faceSwapImage(bson.M{"_id": bson.NewObjectID(), "createdAt": created})
	if faceSwap.ImagePool != "external" || faceSwap.APIProvider != "a2e" {
		t.Fatalf("face swap = %#v", faceSwap)
	}
	tryOn := tryOnImage(bson.M{"_id": bson.NewObjectID(), "createdAt": created})
	if tryOn.ImagePool != "qwen-edit-default" || tryOn.APIProvider != "comfyui" {
		t.Fatalf("try-on = %#v", tryOn)
	}
}

func TestAdminImagePoolFilterUnknownAndResolverFallback(t *testing.T) {
	filter := imagePoolFilter("unknown")
	if filter == nil || filter["imagePool"] == nil || filter["provider"] == nil {
		t.Fatalf("unknown filter = %#v", filter)
	}
	if pool := resolveImagePool("", "", nil, "", "unrecognized-provider", ""); pool != "unknown" {
		t.Fatalf("pool = %q, want unknown", pool)
	}
}
