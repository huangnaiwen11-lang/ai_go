package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/runtimeapp"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoRuntimeAppResolver struct {
	collection *mongo.Collection
}

// NewRuntimeAppResolver creates the read-only runtime App identity resolver.
// It intentionally has no access to the admin App repository or its complete
// App projection, so configuration and secret fields cannot escape this path.
func NewRuntimeAppResolver(data *Data) runtimeapp.Resolver {
	if data == nil || data.database == nil {
		return &mongoRuntimeAppResolver{}
	}
	return &mongoRuntimeAppResolver{collection: data.database.Collection(schema.CollectionApps)}
}

func (resolver *mongoRuntimeAppResolver) Resolve(ctx context.Context, platform, identifier string) (runtimeapp.App, error) {
	input, err := runtimeapp.Normalize(platform, identifier)
	if err != nil {
		return runtimeapp.App{}, err
	}
	if resolver == nil || resolver.collection == nil {
		return runtimeapp.App{}, fmt.Errorf("runtime app resolver unavailable")
	}

	// The bounded two-document preflight is deliberate: iOS and web Apps have
	// legacy identifier representations without a normalized unique key. Never
	// select an arbitrary match; any duplicate remains unavailable at runtime.
	cursor, err := resolver.collection.Find(
		ctx,
		runtimeAppFilter(input),
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetLimit(2).SetCollation(schema.RuntimeAppIdentifierCollation()),
	)
	if err != nil {
		return runtimeapp.App{}, fmt.Errorf("find runtime app: %w", err)
	}
	defer cursor.Close(ctx)

	ids := make([]string, 0, 2)
	for cursor.Next(ctx) {
		var document struct {
			ID any `bson:"_id"`
		}
		if err := cursor.Decode(&document); err != nil {
			return runtimeapp.App{}, fmt.Errorf("decode runtime app: %w", err)
		}
		id := valueString(document.ID)
		if id == "" {
			return runtimeapp.App{}, runtimeapp.ErrUnresolved
		}
		ids = append(ids, id)
	}
	if err := cursor.Err(); err != nil {
		return runtimeapp.App{}, fmt.Errorf("iterate runtime apps: %w", err)
	}
	if len(ids) != 1 {
		return runtimeapp.App{}, runtimeapp.ErrUnresolved
	}
	return runtimeapp.App{ID: ids[0], Platform: input.Platform, Identifier: input.Identifier}, nil
}

func runtimeAppFilter(input runtimeapp.Input) bson.D {
	filter := bson.D{
		{Key: "platform", Value: string(input.Platform)},
		{Key: "status", Value: "active"},
	}

	var fields []string
	switch input.Platform {
	case runtimeapp.PlatformAndroid:
		fields = []string{"clientId", "packageName", "nativeBuild.android.applicationId"}
	case runtimeapp.PlatformIOS:
		fields = []string{"bundleId", "nativeBuild.ios.bundleId"}
	case runtimeapp.PlatformWeb:
		fields = []string{"clientId", "domain"}
	}
	conditions := make(bson.A, 0, len(fields))
	for _, field := range fields {
		conditions = append(conditions, bson.D{{Key: field, Value: input.Identifier}})
	}
	return append(filter, bson.E{Key: "$or", Value: conditions})
}

var _ runtimeapp.Resolver = (*mongoRuntimeAppResolver)(nil)
