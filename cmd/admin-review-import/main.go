// Command admin-review-import builds the Go-owned moderation projection from
// explicitly named legacy Node MongoDB collections.  It is dry-run by default,
// never calls a media provider, and never invents output URLs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	modeDryRun    = "dry-run"
	modeImport    = "import"
	importTimeout = 30 * time.Minute
)

type collectionReport struct {
	Scanned   int64
	Imported  int64
	Orphans   int64
	Conflicts int64
}

type importReport struct {
	RunID       string
	Mode        string
	Collections map[string]collectionReport
	Orphans     int64
	Conflicts   int64
	Imported    int64
	Ready       bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin-review-import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	legacyURI := flags.String("legacy-uri", "", "旧 Node MongoDB URI（必填；不会输出）")
	legacyDatabase := flags.String("legacy-database", "", "旧 Node MongoDB database（必填）")
	targetURI := flags.String("target-uri", "", "Go MongoDB URI（必填；不会输出）")
	targetDatabase := flags.String("target-database", "", "Go MongoDB database（必填）")
	mode := flags.String("mode", modeDryRun, "dry-run | import")
	confirmReady := flags.Bool("confirm-ready", false, "导入无孤儿/冲突后显式写入 projection ready=true")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if strings.TrimSpace(*legacyURI) == "" || strings.TrimSpace(*legacyDatabase) == "" || strings.TrimSpace(*targetURI) == "" || strings.TrimSpace(*targetDatabase) == "" {
		fmt.Fprintln(stderr, "必须显式提供 --legacy-uri、--legacy-database、--target-uri、--target-database。")
		return 2
	}
	if *mode != modeDryRun && *mode != modeImport {
		fmt.Fprintln(stderr, "--mode 必须是 dry-run 或 import。")
		return 2
	}
	if *mode == modeDryRun && *confirmReady {
		fmt.Fprintln(stderr, "dry-run 不允许 --confirm-ready。")
		return 2
	}
	if *legacyURI == *targetURI && *legacyDatabase == *targetDatabase {
		fmt.Fprintln(stderr, "legacy 与 target 不能是同一个 Mongo database。")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()
	legacyClient, err := openClient(ctx, *legacyURI)
	if err != nil {
		fmt.Fprintln(stderr, "连接 legacy MongoDB 失败。")
		return 1
	}
	defer disconnect(legacyClient)
	targetClient, err := openClient(ctx, *targetURI)
	if err != nil {
		fmt.Fprintln(stderr, "连接 target MongoDB 失败。")
		return 1
	}
	defer disconnect(targetClient)

	report, err := importProjection(ctx, legacyClient.Database(*legacyDatabase), targetClient.Database(*targetDatabase), *mode, *confirmReady)
	if err != nil {
		fmt.Fprintln(stderr, "审核投影导入失败：", err)
		return 1
	}
	printReport(stdout, report)
	if report.Orphans != 0 || report.Conflicts != 0 {
		fmt.Fprintln(stderr, "存在 orphan/conflict；projection ready 保持 false。")
		return 1
	}
	return 0
}

func openClient(ctx context.Context, uri string) (*mongo.Client, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return client, nil
}

func disconnect(client *mongo.Client) {
	if client != nil {
		_ = client.Disconnect(context.Background())
	}
}

func importProjection(ctx context.Context, legacy, target *mongo.Database, mode string, confirmReady bool) (importReport, error) {
	if legacy == nil || target == nil {
		return importReport{}, errors.New("Mongo database is not configured")
	}
	if mode != modeDryRun && mode != modeImport {
		return importReport{}, errors.New("invalid mode")
	}
	report := importReport{RunID: uuid.NewString(), Mode: mode, Collections: make(map[string]collectionReport)}
	if mode == modeImport {
		if err := ensureProjectionIndexes(ctx, target); err != nil {
			return report, err
		}
		// Any non-dry run first closes the reader fence.  A failed or partial
		// import must not leave an old ready=true marker advertising a projection
		// whose source has not just been reconciled.
		if _, err := target.Collection(schema.CollectionAdminReviewProjectionState).UpdateOne(ctx,
			bson.M{"_id": "global"}, bson.M{"$set": bson.M{"ready": false, "updated_at": time.Now().UTC(), "run_id": report.RunID}}, options.UpdateOne().SetUpsert(true)); err != nil {
			return report, fmt.Errorf("close projection readiness fence: %w", err)
		}
	}
	if err := requireLegacyCollections(ctx, legacy); err != nil {
		return report, err
	}
	for _, source := range []string{
		adminreview.LegacyGeneratedImages,
		adminreview.LegacyGeneratedVideos,
		adminreview.LegacyAnimates,
		adminreview.LegacyFaceSwapTasks,
	} {
		collection := legacy.Collection(source)
		cursor, err := collection.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return report, fmt.Errorf("scan %s: %w", source, err)
		}
		collectionResult := collectionReport{}
		for cursor.Next(ctx) {
			var document bson.M
			if err := cursor.Decode(&document); err != nil {
				_ = cursor.Close(ctx)
				return report, fmt.Errorf("decode %s: %w", source, err)
			}
			collectionResult.Scanned++
			outcome := adminreview.MapLegacyDocument(source, document)
			switch outcome.Classification {
			case adminreview.ImportReady:
				if mode == modeImport {
					if err := upsertRecord(ctx, target, outcome.Record); err != nil {
						_ = cursor.Close(ctx)
						return report, fmt.Errorf("upsert %s: %w", source, err)
					}
				}
				collectionResult.Imported++
				report.Imported++
			case adminreview.ImportOrphan:
				collectionResult.Orphans++
				report.Orphans++
			case adminreview.ImportConflict:
				collectionResult.Conflicts++
				report.Conflicts++
			}
		}
		if err := cursor.Err(); err != nil {
			_ = cursor.Close(ctx)
			return report, fmt.Errorf("iterate %s: %w", source, err)
		}
		_ = cursor.Close(ctx)
		report.Collections[source] = collectionResult
	}

	if mode == modeImport {
		if err := writeAudit(ctx, target, report, confirmReady); err != nil {
			return report, err
		}
		if report.Orphans == 0 && report.Conflicts == 0 && confirmReady {
			if err := markReady(ctx, target, report); err != nil {
				return report, err
			}
			report.Ready = true
		}
	}
	return report, nil
}

func requireLegacyCollections(ctx context.Context, database *mongo.Database) error {
	names, err := database.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("list legacy collections: %w", err)
	}
	existing := make(map[string]struct{}, len(names))
	for _, name := range names {
		existing[name] = struct{}{}
	}
	for _, source := range []string{
		adminreview.LegacyGeneratedImages,
		adminreview.LegacyGeneratedVideos,
		adminreview.LegacyAnimates,
		adminreview.LegacyFaceSwapTasks,
	} {
		if _, ok := existing[source]; !ok {
			return fmt.Errorf("legacy collection %q is missing", source)
		}
	}
	return nil
}

func ensureProjectionIndexes(ctx context.Context, target *mongo.Database) error {
	indexes := target.Collection(schema.CollectionAdminReviewItems).Indexes()
	_, err := indexes.CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "source", Value: 1}, {Key: "legacy_source_id", Value: 1}},
		Options: options.Index().SetName("ux_admin_review_items_source_legacy").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "legacy_source_id", Value: bson.D{{Key: "$exists", Value: true}}}}),
	})
	return err
}

func upsertRecord(ctx context.Context, target *mongo.Database, record adminreview.ImportRecord) error {
	document := model.AdminReviewItemDocument{
		ID: record.ID, MediaType: record.MediaType, Source: record.Source, LegacySourceID: record.LegacySourceID,
		UserID: record.UserID, AssetID: record.AssetID, OutputRef: record.OutputRef, ReviewStatus: record.ReviewStatus,
		VisibilityStatus: record.VisibilityStatus, GenerationStatus: record.GenerationStatus, Prompt: record.Prompt,
		NegativePrompt: record.NegativePrompt, TemplateID: record.TemplateID, TemplateTitle: record.TemplateTitle,
		ReviewedBy: record.ReviewedBy, ReviewedAt: record.ReviewedAt, RejectReason: record.RejectReason,
		Version: record.Version, CreatedAt: record.CreatedAt, OutputAt: record.OutputAt, UpdatedAt: record.UpdatedAt,
	}
	_, err := target.Collection(schema.CollectionAdminReviewItems).UpdateOne(ctx,
		bson.M{"source": record.Source, "legacy_source_id": record.LegacySourceID},
		bson.M{"$setOnInsert": document}, options.UpdateOne().SetUpsert(true))
	return err
}

func writeAudit(ctx context.Context, target *mongo.Database, report importReport, confirmReady bool) error {
	collections := bson.M{}
	for name, result := range report.Collections {
		collections[name] = bson.M{"scanned": result.Scanned, "imported": result.Imported, "orphans": result.Orphans, "conflicts": result.Conflicts}
	}
	_, err := target.Collection(schema.CollectionAdminAudit).InsertOne(ctx, bson.M{
		"_id": uuid.NewString(), "actor_id": "admin-review-import", "target_id": "admin_review_projection",
		"action": "admin_review_projection_import", "run_id": report.RunID, "mode": report.Mode,
		"confirm_ready": confirmReady, "scanned": reportScanned(report), "imported": report.Imported,
		"orphans": report.Orphans, "conflicts": report.Conflicts, "collections": collections, "created_at": time.Now().UTC(),
	})
	return err
}

func reportScanned(report importReport) int64 {
	var total int64
	for _, result := range report.Collections {
		total += result.Scanned
	}
	return total
}

func markReady(ctx context.Context, target *mongo.Database, report importReport) error {
	_, err := target.Collection(schema.CollectionAdminReviewProjectionState).UpdateOne(ctx,
		bson.M{"_id": "global"}, bson.M{"$set": bson.M{
			"ready": true, "updated_at": time.Now().UTC(), "run_id": report.RunID,
			"source": "legacy-node", "imported": report.Imported,
		}}, options.UpdateOne().SetUpsert(true))
	return err
}

func printReport(writer io.Writer, report importReport) {
	fmt.Fprintf(writer, "审核投影 %s：run=%s scanned=%d imported=%d orphan=%d conflict=%d ready=%t\n", report.Mode, report.RunID, reportScanned(report), report.Imported, report.Orphans, report.Conflicts, report.Ready)
	for _, source := range []string{adminreview.LegacyGeneratedImages, adminreview.LegacyGeneratedVideos, adminreview.LegacyAnimates, adminreview.LegacyFaceSwapTasks} {
		result := report.Collections[source]
		fmt.Fprintf(writer, "  %s scanned=%d imported=%d orphan=%d conflict=%d\n", source, result.Scanned, result.Imported, result.Orphans, result.Conflicts)
	}
}
