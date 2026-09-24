package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/conf"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// NewAdminData opens the explicitly configured staging MongoDB used by the
// admin projection. It deliberately does not initialize the Go-owned schema:
// the staging database is the legacy Node database and creating every local
// collection/index there would mutate data outside the admin contract.
//
// Session/authentication storage continues to use NewData and therefore the
// local cling_main database. Callers must opt in to this constructor only for
// the admin handler.
func NewAdminData(config *conf.Data) (*Data, func(), error) {
	if err := conf.ValidateStagingMongo(config); err != nil {
		return nil, nil, fmt.Errorf("validate admin staging MongoDB config: %w", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return nil, nil, fmt.Errorf("create admin staging MongoDB client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), mongoConnectionTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, fmt.Errorf("ping admin staging MongoDB: %w", err)
	}

	data := &Data{
		client:   client,
		database: client.Database(config.GetMongo().GetDatabase()),
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), mongoConnectionTimeout)
		defer cancel()
		_ = client.Disconnect(ctx)
	}
	return data, cleanup, nil
}
