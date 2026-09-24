package worker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/testsupport"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMongoIntegrationSuite(m)) }

// Production repositories and transactions are exercised against the isolated
// Mongo suite; only the provider endpoint and catalog are local test doubles.
func TestB2BMongoSubmissionRecovery(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("isolated Mongo not configured")
	}
	config := &conf.Data{Mongo: &conf.Data_Mongo{Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true}}
	storage, closeStorage, err := data.NewData(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStorage)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	db := client.Database("cling_main")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrate.NewInitializer(db).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"catalog-retry", "prepared-crash", "reauthorization"} {
		t.Run(scenario, func(t *testing.T) {
			posts, lookups := 0, 0
			f := newB2BWorkerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					lookups++
					if scenario == "reauthorization" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				} else {
					posts++
					w.WriteHeader(http.StatusCreated)
				}
				_, _ = w.Write([]byte(b2bWorkerJobBody("job-"+scenario, "queued")))
			})
			creationID, userID := uuid.NewString(), uuid.NewString()
			eventID := outbox.SubmissionEventID(b2bWorkerStepID)
			route := b2bWorkerRoute()
			rows := []struct {
				collection, id string
				value          any
			}{
				{schema.CollectionUsers, userID, model.UserDocument{ID: userID, ContentAccess: identity.ContentAccessStandard}},
				{schema.CollectionCreations, creationID, model.CreationDocument{ID: creationID, UserID: userID, IdempotencyKey: uuid.NewString(), Status: string(creations.CreationStatusPendingSubmission)}},
				{schema.CollectionReservations, "reservation:" + creationID, model.ReservationDocument{ID: "reservation:" + creationID, CreationID: creationID, UserID: userID, Status: "reserved"}},
				{schema.CollectionCreationSteps, b2bWorkerStepID, model.CreationStepDocument{ID: b2bWorkerStepID, CreationID: creationID, Sequence: 1, Atom: "text_to_image", SubmitStatus: "ready", Provider: route.Provider, AccountRef: route.AccountRef, ContractVersion: route.ContractVersion, MappingVersion: route.MappingVersion}},
				{schema.CollectionOutboxEvents, eventID, model.OutboxEventDocument{ID: eventID, AggregateID: creationID, EventType: string(outbox.EventTypeGenerationSubmission), Payload: b2bWorkerRecipePayload(t), DeliveryStatus: "pending", NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}},
			}
			for _, row := range rows {
				t.Cleanup(func() {
					_, e := db.Collection(row.collection).DeleteOne(context.Background(), bson.M{"_id": row.id})
					if e != nil {
						t.Error(e)
					}
				})
				if _, err := db.Collection(row.collection).InsertOne(ctx, row.value); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_, _ = db.Collection(schema.CollectionOutboxEvents).DeleteOne(context.Background(), bson.M{"_id": generation.ProviderInboxRecoveryEventID(b2bWorkerStepID)})
			})
			repo := data.NewGenerationSubmissionRepository(storage)
			tx := data.NewTxRunner(storage)
			events := data.NewOutboxRepository(storage)
			w := NewGenerationSubmissionWorker(events, repo, f.reverser, tx, f.router, f.reviewer, "mongo-recovery", func() time.Time { return f.now })
			if scenario == "catalog-retry" {
				f.catalogs.err = errors.New("catalog temporarily unavailable")
				if err := w.DeliverOnce(ctx, eventID); err != nil {
					t.Fatal(err)
				}
				f.catalogs.err = nil
				f.now = f.now.Add(time.Minute)
			} else {
				event, err := events.ClaimByIDAndType(ctx, "mongo-recovery", eventID, outbox.EventTypeGenerationSubmission, f.now, f.now.Add(time.Minute))
				if err != nil || event == nil {
					t.Fatalf("claim: %v", err)
				}
				record, err := repo.ClaimedSubmission(ctx, eventID)
				if err != nil {
					t.Fatal(err)
				}
				request, err := mapB2BRequest(ctx, f.router.config, record)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.prepareB2BSubmission(ctx, record, request, f.now); err != nil {
					t.Fatal(err)
				}
				f.now = f.now.Add(reauthorizationGracePeriod + time.Minute)
			}
			// A fresh worker models restart after an expired lease or safe requeue.
			w = NewGenerationSubmissionWorker(events, repo, f.reverser, tx, f.router, f.reviewer, "mongo-recovered", func() time.Time { return f.now })
			if scenario == "reauthorization" {
				// Exercise the storage mechanism only; runtime replay remains disabled by R4.
				w.reauthorization = repo.(generation.ProviderReauthorizationStore)
			}
			if err := w.DeliverOnce(ctx, eventID); err != nil {
				t.Fatal(err)
			}
			var step bson.M
			if err := db.Collection(schema.CollectionCreationSteps).FindOne(ctx, bson.M{"_id": b2bWorkerStepID}).Decode(&step); err != nil {
				t.Fatal(err)
			}
			if step["external_execution_id"] != "job-"+scenario {
				t.Fatalf("job not bound: %v", step["submit_status"])
			}
			if scenario == "prepared-crash" && (posts != 0 || lookups != 1) {
				t.Fatalf("replayed uncertain request: posts=%d lookups=%d", posts, lookups)
			}
			if scenario != "prepared-crash" && posts != 1 {
				t.Fatalf("posts=%d", posts)
			}
			var event model.OutboxEventDocument
			if err := db.Collection(schema.CollectionOutboxEvents).FindOne(ctx, bson.M{"_id": eventID}).Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event.DeliveryStatus != "delivered" || event.LeaseToken != "" {
				t.Fatal("successful binding did not settle submission lease")
			}
			if scenario == "reauthorization" {
				count, err := db.Collection(schema.CollectionCreationSteps).CountDocuments(ctx, bson.M{"_id": b2bWorkerStepID, "submission_intent.reauthorization.reason": generation.ReauthorizationReasonLookupNotFound})
				if err != nil || count != 1 {
					t.Fatalf("reauthorization audit missing: count=%d err=%v", count, err)
				}
			}
			if f.reverser.calls != 0 {
				t.Fatal("unexpected refund")
			}
		})
	}
}
