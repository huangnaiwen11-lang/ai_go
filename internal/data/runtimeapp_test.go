package data

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"ai-business-service/internal/biz/runtimeapp"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func newRuntimeAppTestResolver(t *testing.T) (runtimeapp.Resolver, *mongo.Database) {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("runtime_app_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = database.Drop(context.Background()) })
	return NewRuntimeAppResolver(&Data{client: client, database: database}), database
}

func TestRuntimeAppResolverResolvesActiveSupportedIdentifiers(t *testing.T) {
	resolver, database := newRuntimeAppTestResolver(t)
	documents := []bson.M{
		{"_id": bson.NewObjectID(), "platform": "android", "status": "active", "clientId": "COM.EXAMPLE.ANDROID.CLIENT", "serverIntegrations": bson.M{"apiKey": "must-not-leak"}},
		{"_id": bson.NewObjectID(), "platform": "android", "status": "active", "packageName": "COM.EXAMPLE.ANDROID.PACKAGE"},
		{"_id": bson.NewObjectID(), "platform": "android", "status": "active", "nativeBuild": bson.M{"android": bson.M{"applicationId": "COM.EXAMPLE.ANDROID.NATIVE"}}},
		{"_id": bson.NewObjectID(), "platform": "ios", "status": "active", "bundleId": "COM.EXAMPLE.IOS.TOP"},
		{"_id": bson.NewObjectID(), "platform": "ios", "status": "active", "nativeBuild": bson.M{"ios": bson.M{"bundleId": "COM.EXAMPLE.IOS.NATIVE"}}},
		{"_id": bson.NewObjectID(), "platform": "web", "status": "active", "clientId": "WEB.CLIENT.EXAMPLE.TEST"},
		{"_id": bson.NewObjectID(), "platform": "web", "status": "active", "domain": "WEB.DOMAIN.EXAMPLE.TEST"},
	}
	if _, err := database.Collection("apps").InsertMany(context.Background(), documents); err != nil {
		t.Fatalf("insert apps: %v", err)
	}

	tests := []struct {
		name       string
		platform   string
		identifier string
		id         string
	}{
		{name: "android client ID", platform: " Android ", identifier: " com.example.android.client ", id: documents[0]["_id"].(bson.ObjectID).Hex()},
		{name: "android package name", platform: "android", identifier: "com.example.android.package", id: documents[1]["_id"].(bson.ObjectID).Hex()},
		{name: "android native build application ID", platform: "android", identifier: "com.example.android.native", id: documents[2]["_id"].(bson.ObjectID).Hex()},
		{name: "ios bundle ID", platform: "ios", identifier: "com.example.ios.top", id: documents[3]["_id"].(bson.ObjectID).Hex()},
		{name: "ios native build bundle ID", platform: "ios", identifier: "com.example.ios.native", id: documents[4]["_id"].(bson.ObjectID).Hex()},
		{name: "web client ID", platform: "web", identifier: "web.client.example.test", id: documents[5]["_id"].(bson.ObjectID).Hex()},
		{name: "web domain", platform: "web", identifier: "web.domain.example.test", id: documents[6]["_id"].(bson.ObjectID).Hex()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := resolver.Resolve(context.Background(), tt.platform, tt.identifier)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			want := runtimeapp.App{ID: tt.id, Platform: runtimeapp.Platform(strings.ToLower(strings.TrimSpace(tt.platform))), Identifier: strings.ToLower(strings.TrimSpace(tt.identifier))}
			if app != want {
				t.Fatalf("Resolve() = %#v, want %#v", app, want)
			}
		})
	}
}

func TestRuntimeAppResolverFailsClosedForInvalidInactiveUnknownAndAmbiguousApps(t *testing.T) {
	resolver, database := newRuntimeAppTestResolver(t)
	if _, err := database.Collection("apps").InsertMany(context.Background(), []bson.M{
		{"_id": bson.NewObjectID(), "platform": "ios", "status": "suspended", "bundleId": "com.example.inactive"},
		{"_id": bson.NewObjectID(), "platform": "web", "status": "active", "clientId": "com.example.ambiguous"},
		{"_id": bson.NewObjectID(), "platform": "web", "status": "active", "domain": "COM.EXAMPLE.AMBIGUOUS"},
		{"_id": bson.NewObjectID(), "platform": "web", "status": "active", "clientId": "com.example.exact-suffix"},
	}); err != nil {
		t.Fatalf("insert apps: %v", err)
	}

	for _, tt := range []struct {
		name       string
		platform   string
		identifier string
	}{
		{name: "invalid platform", platform: "desktop", identifier: "com.example.app"},
		{name: "invalid identifier", platform: "web", identifier: " "},
		{name: "inactive app", platform: "ios", identifier: "com.example.inactive"},
		{name: "unknown app", platform: "android", identifier: "com.example.unknown"},
		{name: "ambiguous app", platform: "web", identifier: "COM.EXAMPLE.AMBIGUOUS"},
		{name: "prefix is not exact", platform: "web", identifier: "com.example.exact"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app, err := resolver.Resolve(context.Background(), tt.platform, tt.identifier)
			if !errors.Is(err, runtimeapp.ErrUnresolved) {
				t.Fatalf("Resolve() error = %v, want ErrUnresolved", err)
			}
			if app != (runtimeapp.App{}) {
				t.Fatalf("Resolve() app = %#v, want zero App on failure", app)
			}
		})
	}
}

func TestRuntimeAppProjectionContainsNoConfigurationFields(t *testing.T) {
	typeOfApp := reflect.TypeOf(runtimeapp.App{})
	want := []string{"ID", "Platform", "Identifier"}
	if typeOfApp.NumField() != len(want) {
		t.Fatalf("runtimeapp.App field count = %d, want %d", typeOfApp.NumField(), len(want))
	}
	for index, name := range want {
		if got := typeOfApp.Field(index).Name; got != name {
			t.Fatalf("runtimeapp.App field %d = %q, want %q", index, got, name)
		}
	}
}

func TestRuntimeAppFilterUsesExactNormalizedIdentifier(t *testing.T) {
	for _, input := range []runtimeapp.Input{
		{Platform: runtimeapp.PlatformAndroid, Identifier: "com.example.runtime"},
		{Platform: runtimeapp.PlatformIOS, Identifier: "com.example.runtime"},
		{Platform: runtimeapp.PlatformWeb, Identifier: "com.example.runtime"},
	} {
		t.Run(string(input.Platform), func(t *testing.T) {
			filter := runtimeAppFilter(input)

			if got, want := filter[:2], (bson.D{{Key: "platform", Value: string(input.Platform)}, {Key: "status", Value: "active"}}); !reflect.DeepEqual(got, want) {
				t.Fatalf("runtimeAppFilter() prefix = %#v, want %#v", got, want)
			}
			conditions, ok := filter[2].Value.(bson.A)
			if !ok {
				t.Fatalf("runtimeAppFilter() conditions = %#v, want bson.A", filter[2].Value)
			}
			for _, raw := range conditions {
				condition, ok := raw.(bson.D)
				if !ok || len(condition) != 1 {
					t.Fatalf("runtimeAppFilter() condition = %#v, want one-field bson.D", raw)
				}
				if got, want := condition[0].Value, input.Identifier; got != want {
					t.Errorf("runtimeAppFilter() %s = %#v, want exact normalized identifier %q", condition[0].Key, got, want)
				}
			}
		})
	}
}
