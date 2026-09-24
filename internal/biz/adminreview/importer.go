package adminreview

// This file contains the legacy moderation projection mapper.  It deliberately
// accepts untyped Mongo documents: the legacy Node schemas are not a Go
// dependency and have changed shape over time.  The importer must therefore be
// conservative and classify missing/contradictory facts instead of inventing
// an output URL or a review decision.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	LegacyGeneratedImages = "generatedimages"
	LegacyGeneratedVideos = "generatedvideos"
	LegacyAnimates        = "animates"
	LegacyFaceSwapTasks   = "faceswaptasks"
)

type ImportClassification string

const (
	ImportReady    ImportClassification = "ready"
	ImportOrphan   ImportClassification = "orphan"
	ImportConflict ImportClassification = "conflict"
)

// ImportRecord is the stable, Go-owned representation written to
// admin_review_items.  It mirrors model.AdminReviewItemDocument without making
// the business package depend on the data package.
type ImportRecord struct {
	ID               string
	MediaType        string
	Source           string
	LegacySourceID   string
	UserID           string
	AssetID          string
	OutputRef        string
	ReviewStatus     string
	VisibilityStatus string
	GenerationStatus string
	Prompt           string
	NegativePrompt   string
	TemplateID       string
	TemplateTitle    string
	ReviewedBy       string
	ReviewedAt       time.Time
	RejectReason     string
	Version          int64
	CreatedAt        time.Time
	OutputAt         time.Time
	UpdatedAt        time.Time
}

type ImportOutcome struct {
	Classification ImportClassification
	Reason         string
	Record         ImportRecord
}

// MapLegacyDocument maps one Node document.  The source argument must be one
// of the four exported collection constants.  IDs are hashed into a stable
// Mongo _id so a rerun is idempotent even when a legacy ID contains a special
// character.
func MapLegacyDocument(source string, document bson.M) ImportOutcome {
	if document == nil {
		return ImportOutcome{Classification: ImportOrphan, Reason: "nil document"}
	}
	id := scalarString(document["_id"])
	if id == "" {
		return ImportOutcome{Classification: ImportOrphan, Reason: "missing _id"}
	}
	if !validLegacySource(source) {
		return ImportOutcome{Classification: ImportConflict, Reason: "unsupported source collection"}
	}

	mediaType, outputRef := mediaAndOutput(source, document)
	if mediaType == "" {
		return ImportOutcome{Classification: ImportConflict, Reason: "unsupported media type"}
	}
	if outputRef == "" {
		return ImportOutcome{Classification: ImportOrphan, Reason: "missing output media reference"}
	}
	userID := scalarString(document["userId"])
	if userID == "" {
		return ImportOutcome{Classification: ImportOrphan, Reason: "missing userId"}
	}
	createdAt := timeValue(document["createdAt"])
	if createdAt.IsZero() {
		return ImportOutcome{Classification: ImportOrphan, Reason: "missing createdAt"}
	}
	updatedAt := timeValue(document["updatedAt"])
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	reviewStatus, visibilityStatus, reason, classification := reviewState(source, document)
	if classification != ImportReady {
		return ImportOutcome{Classification: classification, Reason: reason}
	}
	rawGenerationStatus := scalarString(document["generationStatus"])
	generationStatus := normalizeGenerationStatus(rawGenerationStatus)
	if source == LegacyAnimates {
		generationStatus = normalizeAnimateStatus(scalarString(document["status"]))
	}
	// Older GeneratedImage/GeneratedVideo/FaceSwapTask rows predate the
	// generationStatus field.  A real output reference is sufficient evidence
	// that the lifecycle reached a terminal success; this is not a review
	// decision and does not auto-approve the item.
	if generationStatus == "" && rawGenerationStatus == "" && source != LegacyAnimates && outputRef != "" {
		generationStatus = GenerationStatusSucceeded
	}
	if generationStatus == "" {
		return ImportOutcome{Classification: ImportConflict, Reason: "missing generation status"}
	}

	record := ImportRecord{
		ID:               stableID(source, id),
		MediaType:        mediaType,
		Source:           "legacy." + source,
		LegacySourceID:   id,
		UserID:           userID,
		AssetID:          id,
		OutputRef:        outputRef,
		ReviewStatus:     reviewStatus,
		VisibilityStatus: visibilityStatus,
		GenerationStatus: generationStatus,
		Prompt:           firstString(document, "prompt", "userPrompt"),
		NegativePrompt:   scalarString(document["negativePrompt"]),
		TemplateID:       scalarString(document["templateId"]),
		TemplateTitle:    firstString(document, "templateTitle", "name"),
		ReviewedBy:       scalarString(document["reviewedBy"]),
		ReviewedAt:       timeValue(document["reviewedAt"]),
		RejectReason:     scalarString(document["rejectReason"]),
		Version:          1,
		CreatedAt:        createdAt,
		OutputAt:         firstTime(document, "completedAt", "updatedAt", "createdAt"),
		UpdatedAt:        updatedAt,
	}
	return ImportOutcome{Classification: ImportReady, Record: record}
}

func validLegacySource(source string) bool {
	switch source {
	case LegacyGeneratedImages, LegacyGeneratedVideos, LegacyAnimates, LegacyFaceSwapTasks:
		return true
	default:
		return false
	}
}

func stableID(source, id string) string {
	hash := sha256.Sum256([]byte(source + "\x00" + id))
	return "legacy-review-" + hex.EncodeToString(hash[:])
}

func mediaAndOutput(source string, document bson.M) (string, string) {
	switch source {
	case LegacyGeneratedImages:
		return MediaTypeImage, scalarString(document["imageUrl"])
	case LegacyGeneratedVideos:
		return MediaTypeVideo, scalarString(document["videoUrl"])
	case LegacyAnimates:
		return MediaTypeVideo, scalarString(document["resultUrl"])
	case LegacyFaceSwapTasks:
		switch strings.ToLower(scalarString(document["type"])) {
		case "", "video":
			return MediaTypeVideo, scalarString(document["resultVideoUrl"])
		case "image":
			return MediaTypeImage, scalarString(document["resultImageUrl"])
		default:
			return "", ""
		}
	default:
		return "", ""
	}
}

func reviewState(source string, document bson.M) (string, string, string, ImportClassification) {
	if source == LegacyAnimates {
		// Animate has no moderation state in the Node schema.  Completed output
		// is intentionally imported as pending, never auto-approved.
		return ReviewStatusPending, visibilityFromBool(document["isPublic"], "public"), "", ImportReady
	}
	status := strings.ToLower(strings.TrimSpace(scalarString(document["status"])))
	if status == "" {
		return ReviewStatusPending, visibilityFromBool(document["isPublic"], "public"), "", ImportReady
	}
	switch status {
	case ReviewStatusPending, ReviewStatusApproved, ReviewStatusRejected, ReviewStatusDeleted:
		return status, visibilityFromBool(document["isPublic"], visibilityForReview(status)), "", ImportReady
	case "active":
		return ReviewStatusApproved, "public", "", ImportReady
	case "hidden":
		return ReviewStatusRejected, "private", "", ImportReady
	default:
		return "", "", "unsupported review status", ImportConflict
	}
}

func visibilityForReview(status string) string {
	if status == ReviewStatusApproved {
		return "public"
	}
	return "private"
}

func visibilityFromBool(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	if v, ok := value.(bool); ok {
		if v {
			return "public"
		}
		return "private"
	}
	return fallback
}

func normalizeGenerationStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "completed", "succeeded", "success":
		return GenerationStatusSucceeded
	case "generating", "processing", "pending", "retrying":
		return GenerationStatusGenerating
	case "failed", "error":
		return GenerationStatusFailed
	case "cancelled", "canceled":
		return GenerationStatusCancelled
	default:
		return ""
	}
}

func normalizeAnimateStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "completed":
		return GenerationStatusSucceeded
	case "pending", "processing":
		return GenerationStatusGenerating
	case "failed":
		return GenerationStatusFailed
	case "cancelled", "canceled":
		return GenerationStatusCancelled
	default:
		return ""
	}
}

func firstString(document bson.M, keys ...string) string {
	for _, key := range keys {
		if value := scalarString(document[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstTime(document bson.M, keys ...string) time.Time {
	for _, key := range keys {
		if value := timeValue(document[key]); !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func scalarString(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case bson.ObjectID:
		return value.Hex()
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func timeValue(value any) time.Time {
	switch value := value.(type) {
	case time.Time:
		return value
	case bson.DateTime:
		return value.Time()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	}
	return time.Time{}
}
