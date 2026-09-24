package data

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var defaultDomainTLDs = []string{"com", "app", "ai", "co", "io", "xyz", "site", "online", "dev", "net"}

func (r *mongoAdminAppsRepository) platformCollection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("platform config repository unavailable")
	}
	return r.data.database.Collection(schema.CollectionPlatformConfigs), nil
}

func (r *mongoAdminAppsRepository) appDocument(ctx context.Context, id string) (bson.M, adminapps.Document, error) {
	collection, err := r.collection()
	if err != nil {
		return nil, nil, err
	}
	var raw bson.M
	if err := collection.FindOne(ctx, appIDFilter(strings.TrimSpace(id))).Decode(&raw); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil, adminapps.ErrNotFound
		}
		return nil, nil, fmt.Errorf("get app: %w", err)
	}
	return raw, adminapps.Document(documentFromRaw(raw)), nil
}

func (r *mongoAdminAppsRepository) updateAppDocument(ctx context.Context, actor adminapps.Actor, id, action string, set bson.M, unset bson.M) (adminapps.App, error) {
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, err
	}
	set["updatedAt"] = adminAppsNow()
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	var updated bson.M
	err = collection.FindOneAndUpdate(ctx, appIDFilter(id), update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return adminapps.App{}, adminapps.ErrNotFound
	}
	if err != nil {
		return adminapps.App{}, fmt.Errorf("%s app: %w", action, err)
	}
	if err := r.audit(ctx, actor, action, id, adminAppsNow()); err != nil {
		return adminapps.App{}, err
	}
	return appFromDocument(updated), nil
}

func nestedDocument(value any) adminapps.Document {
	switch result := value.(type) {
	case map[string]any:
		return adminapps.Document(result)
	case adminapps.Document:
		return result
	}
	return adminapps.Document{}
}

func (r *mongoAdminAppsRepository) UpdatePackageConfig(ctx context.Context, actor adminapps.Actor, id string, payload map[string]any) (adminapps.App, adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return adminapps.App{}, nil, err
	}
	config := nestedDocument(payload["config"])
	files := nestedDocument(payload["files"])
	android, ios := nestedDocument(config["android"]), nestedDocument(config["ios"])
	facebook, google := nestedDocument(config["facebook"]), nestedDocument(config["google"])
	native := adminapps.Document{
		"appKey": stringValue(payload["appKey"]), "displayName": stringValue(config["displayName"]),
		"android":  adminapps.Document{"applicationId": stringValue(android["applicationId"]), "namespace": stringValue(android["namespace"]), "deepLinkSchemes": stringListForStore(android["deepLinkSchemes"]), "signingCertificateSha256": stringListForStore(android["signingCertificateSha256"])},
		"ios":      adminapps.Document{"bundleId": stringValue(ios["bundleId"])},
		"facebook": adminapps.Document{"appId": stringValue(facebook["appId"]), "clientToken": stringValue(facebook["clientToken"])},
		"google":   adminapps.Document{"iosClientId": stringValue(google["iosClientId"]), "iosReversedClientId": stringValue(google["iosReversedClientId"]), "androidClientId": stringValue(google["androidClientId"]), "serverClientId": stringValue(google["serverClientId"])},
		"firebase": adminapps.Document{"androidGoogleServicesJson": stringValue(files["androidGoogleServicesJson"]), "iosGoogleServiceInfoPlist": stringValue(files["iosGoogleServiceInfoPlist"])},
	}
	if native["appKey"] == "" {
		native["appKey"] = adminapps.FieldString(app, "appKey")
	}
	if native["displayName"] == "" {
		native["displayName"] = adminapps.FieldString(app, "name")
	}
	effective := adminapps.Merge(app, adminapps.Document{"appKey": native["appKey"], "packageName": adminapps.PathString(native, "android", "applicationId"), "bundleId": adminapps.PathString(native, "ios", "bundleId"), "nativeBuild": native})
	if _, err := adminapps.BuildPackageConfig(effective); err != nil {
		return adminapps.App{}, nil, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, nil, err
	}
	if err := r.assertNativeIdentifiersFree(ctx, collection, effective, objectIDOrNil(id)); err != nil {
		return adminapps.App{}, nil, err
	}
	packageConfig := adminapps.Document{"appKey": native["appKey"], "config": config, "files": adminapps.Document{"androidGoogleServicesJson": configuredMarker(files["androidGoogleServicesJson"]), "iosGoogleServiceInfoPlist": configuredMarker(files["iosGoogleServiceInfoPlist"])}, "updatedAt": adminAppsNow(), "updatedBy": actor.ID}
	set := bson.M{"appKey": native["appKey"], "packageName": effective["packageName"], "bundleId": effective["bundleId"], "nativeBuild": native, "packageConfig": packageConfig, "nativeIdentifiers": adminapps.NativeIdentifiers(effective)}
	if store, ok := payload["storeSubmission"].(map[string]any); ok {
		set["storeSubmission"] = mergeStoreSubmission(app, store)
	}
	updated, err := r.updateAppDocument(ctx, actor, id, "app_package_config_update", set, nil)
	if err != nil {
		return adminapps.App{}, nil, err
	}
	export, err := adminapps.BuildPackageConfig(adminapps.Document(updated.Fields))
	if err != nil {
		return adminapps.App{}, nil, err
	}
	return updated, export, nil
}

func stringValue(value any) string { text, _ := value.(string); return strings.TrimSpace(text) }
func configuredMarker(value any) any {
	if stringValue(value) != "" {
		return "configured"
	}
	return nil
}
func stringListForStore(value any) []any {
	rows, _ := value.([]any)
	result := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		text := stringValue(row)
		if text != "" && !seen[text] {
			seen[text] = true
			result = append(result, text)
		}
	}
	return result
}
func mergeStoreSubmission(app adminapps.Document, patch map[string]any) adminapps.Document {
	result := nestedDocument(app["storeSubmission"])
	if result == nil {
		result = adminapps.Document{}
	}
	now := adminAppsNow()
	for _, platform := range []string{"android", "ios"} {
		if value, ok := patch[platform].(map[string]any); ok {
			status := stringValue(value["status"])
			if containsStoreStatus(status) {
				result[platform] = adminapps.Document{"status": status, "updatedAt": now}
			}
		}
	}
	return result
}
func containsStoreStatus(value string) bool {
	for _, item := range []string{"draft", "staging_ready", "uploaded", "in_review", "rejected", "approved", "live"} {
		if value == item {
			return true
		}
	}
	return false
}

func (r *mongoAdminAppsRepository) NativeConfigTemplate(ctx context.Context, id string) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	template := adminapps.BuildNativeConfigTemplate(app)
	if native := adminapps.FieldObject(app, "nativeConfig"); native != nil {
		template = adminapps.MergeDeep(template, native)
	}
	return template, nil
}

func (r *mongoAdminAppsRepository) SetReviewMode(ctx context.Context, actor adminapps.Actor, id string, input map[string]any) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	platform, clientID, identifier, ok := adminapps.NativeRuntimeTarget(app)
	if !ok {
		return nil, fmt.Errorf("%w: Review mode can only be toggled for a configured iOS or Android app", adminapps.ErrInvalid)
	}
	review, err := adminapps.NormalizeReviewMode(input)
	if err != nil {
		return nil, err
	}
	configs, err := r.platformCollection()
	if err != nil {
		return nil, err
	}
	now := adminAppsNow()
	configSet := bson.M{"platform": platform, "clientId": clientID, "displayName": firstNonEmptyCompat(adminapps.PathString(app, "nativeBuild", "displayName"), adminapps.FieldString(app, "name")), "enabled": true, "clientIdentifiers": platformIdentifiers(platform, identifier), "reviewMode": review, "updatedBy": actor.ID, "updatedAt": now, "changeReason": firstNonEmptyCompat(stringValue(input["changeReason"]), "App package factory review-mode update")}
	if review["mode"] == "strict" {
		configSet["contentPolicy"] = adminapps.SafeContentPolicy()
	}
	if _, err := configs.UpdateOne(ctx, bson.M{"platform": platform, "clientId": clientID}, bson.M{"$set": configSet}, options.UpdateOne().SetUpsert(true)); err != nil {
		return nil, fmt.Errorf("set review platform config: %w", err)
	}
	result := adminapps.Document{"appId": id, "platform": platform, "clientId": clientID, "appIdentifier": identifier, "reviewMode": review}
	if platform == "ios" {
		native := adminapps.FieldObject(app, "nativeConfig")
		if native == nil {
			native = adminapps.Document{}
		}
		rules := adminapps.Document{"defaultS": 1, "rules": []any{}}
		enabled, _ := review["enabled"].(bool)
		if enabled {
			rules["defaultS"] = 0
		}
		native["stateRules"] = rules
		if _, err := r.updateAppDocument(ctx, actor, id, "app_review_mode_update", bson.M{"nativeConfig": native}, nil); err != nil {
			return nil, err
		}
		result["nativeStateRules"] = rules
	} else if err := r.audit(ctx, actor, "app_review_mode_update", id, now); err != nil {
		return nil, err
	}
	if review["mode"] == "strict" {
		result["contentPolicy"] = adminapps.SafeContentPolicy()
	}
	return result, nil
}

func platformIdentifiers(platform, identifier string) adminapps.Document {
	result := adminapps.Document{"domains": []any{}, "bundleIds": []any{}, "packageNames": []any{}}
	if platform == "ios" {
		result["bundleIds"] = []any{identifier}
	}
	if platform == "android" {
		result["packageNames"] = []any{identifier}
	}
	return result
}

func (r *mongoAdminAppsRepository) IntegrationCheck(ctx context.Context, id string) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	checks := []any{}
	native := adminapps.FieldString(app, "platform") == "ios" || adminapps.FieldString(app, "platform") == "android"
	var pkg any
	if native {
		export, e := adminapps.BuildPackageConfig(app)
		if e != nil {
			checks = append(checks, check("packageConfig", false, e.Error(), nil))
		} else {
			pkg = export
			checks = append(checks, check("packageConfig", true, "Package config can be exported for native client builds", nil))
			google := adminapps.FieldObject(adminapps.FieldObject(export, "config"), "google")
			checks = append(checks, check("oauth", adminapps.FieldString(google, "androidClientId") != "" && adminapps.FieldString(google, "serverClientId") != "", "Google OAuth client ids are configured", nil))
			files := adminapps.FieldObject(export, "files")
			checks = append(checks, check("firebase", adminapps.FieldString(files, "androidGoogleServicesJson") != "" || adminapps.FieldString(files, "iosGoogleServiceInfoPlist") != "", "Firebase client config file is present", nil))
		}
	} else {
		checks = append(checks, check("webBoundary", true, "Web apps do not require native package, Firebase, Google Play, or RTDN integration", nil))
	}
	platform, clientID, identifier, ok := adminapps.NativeRuntimeTarget(app)
	if !ok && adminapps.FieldString(app, "platform") == "web" {
		platform = "web"
		clientID = firstNonEmptyCompat(adminapps.FieldString(app, "clientId"), adminapps.NormalizeWebDomain(adminapps.FieldString(app, "domain")))
		identifier = clientID
		ok = identifier != ""
	}
	var config adminapps.Document
	if ok {
		config, _ = r.GetPlatformConfig(ctx, platform, clientID)
		checks = append(checks, check("platformConfig", config != nil, "PlatformConfig resolves for this app identifier", adminapps.Document{"clientId": clientID, "platform": platform, "appIdentifier": identifier}))
	} else {
		checks = append(checks, check("clientHeaders", false, "Missing bundleId/packageName/domain for X-Client-App-Id mapping", nil))
	}
	good := true
	for _, item := range checks {
		if entry, ok := item.(adminapps.Document); ok && entry["status"] == "fail" {
			good = false
		}
	}
	return adminapps.Document{"ok": good, "appId": id, "platform": platform, "clientId": clientID, "appIdentifier": identifier, "packageConfig": pkg, "checks": checks}, nil
}
func check(key string, ok bool, message string, extra adminapps.Document) adminapps.Document {
	status := "fail"
	if ok {
		status = "pass"
	}
	result := adminapps.Document{"key": key, "status": status, "message": message}
	for k, v := range extra {
		result[k] = v
	}
	return result
}

func (r *mongoAdminAppsRepository) CreateBuildRequest(ctx context.Context, actor adminapps.Actor, id string, input map[string]any) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	platform := firstNonEmptyCompat(stringValue(input["platform"]), adminapps.FieldString(app, "platform"))
	if platform != "android" && platform != "ios" {
		return nil, fmt.Errorf("%w: Build requests only support android or ios packages", adminapps.ErrInvalid)
	}
	_, _, identifier, ok := adminapps.NativeRuntimeTarget(adminapps.Merge(app, adminapps.Document{"platform": platform}))
	if !ok {
		return nil, fmt.Errorf("%w: Missing package identifier for %s", adminapps.ErrInvalid, platform)
	}
	channel := firstNonEmptyCompat(stringValue(input["channel"]), map[string]string{"ios": "testflight", "android": "play_internal"}[platform])
	if !containsChannel(channel) {
		return nil, fmt.Errorf("%w: invalid build channel", adminapps.ErrInvalid)
	}
	now := adminAppsNow()
	request := adminapps.Document{"requestId": "build_" + uuid.NewString(), "platform": platform, "channel": channel, "status": "requested", "ciTrigger": "pending", "requestedBy": actor.ID, "requestedAt": now}
	if notes := stringValue(input["notes"]); notes != "" {
		request["notes"] = notes
	}
	store := nestedDocument(app["storeSubmission"])
	requests := adminapps.FieldArray(store, "buildRequests")
	requests = append(requests, request)
	entry := nestedDocument(store[platform])
	entry["status"] = firstNonEmptyCompat(adminapps.FieldString(entry, "status"), "staging_ready")
	entry["lastBuildRequestId"] = request["requestId"]
	entry["updatedAt"] = now
	store[platform] = entry
	store["buildRequests"] = requests
	if _, err := r.updateAppDocument(ctx, actor, id, "app_build_request", bson.M{"storeSubmission": store}, nil); err != nil {
		return nil, err
	}
	return adminapps.Document{"appId": id, "buildRequest": request, "appIdentifier": identifier}, nil
}
func containsChannel(value string) bool {
	for _, x := range []string{"staging", "internal", "play_internal", "testflight", "manual"} {
		if value == x {
			return true
		}
	}
	return false
}

func (r *mongoAdminAppsRepository) APIDomainCandidates(ctx context.Context, id string, input map[string]any) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	tlds := allowedTLDs(input)
	seed := adminapps.DomainCandidateSeed(app, input)
	limit := intValue(input["limit"], 10)
	if limit < 1 || limit > 20 {
		return nil, fmt.Errorf("%w: limit must be 1..20", adminapps.ErrInvalid)
	}
	candidates := make([]any, 0, limit)
	for _, tld := range tlds {
		if len(candidates) == limit {
			break
		}
		candidates = append(candidates, adminapps.Document{"domain": seed + "." + tld, "registrationCost": nil, "renewalCost": nil, "currency": "USD", "tier": "unverified"})
	}
	return adminapps.Document{"appId": id, "appName": adminapps.FieldString(app, "name"), "seed": seed, "currentApiUrl": adminapps.ResolveAPIURL(app), "allowedTlds": stringToAny(tlds), "candidates": candidates, "source": "local_suggestion_only"}, nil
}
func allowedTLDs(input map[string]any) []string {
	value := input["allowedTlds"]
	if value == nil {
		value = input["extensions"]
	}
	rows, _ := value.([]any)
	result := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		text := strings.TrimPrefix(strings.ToLower(stringValue(row)), ".")
		if text != "" && !seen[text] {
			seen[text] = true
			result = append(result, text)
		}
	}
	if len(result) == 0 {
		return append([]string(nil), defaultDomainTLDs...)
	}
	return result
}
func intValue(value any, defaultValue int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return defaultValue
	}
}
func stringToAny(values []string) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

func (r *mongoAdminAppsRepository) RegisterAPIDomain(ctx context.Context, actor adminapps.Actor, id string, input map[string]any) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	if stringValue(input["confirmation"]) != "REGISTER_DOMAIN" {
		return nil, fmt.Errorf("%w: confirmation must be REGISTER_DOMAIN", adminapps.ErrInvalid)
	}
	if adminapps.FieldString(app, "status") == "deprecated" {
		return nil, fmt.Errorf("%w: deprecated app cannot provision API domain", adminapps.ErrInvalid)
	}
	domain, err := adminapps.NormalizedDomain(stringValue(input["domain"]))
	if err != nil {
		return nil, err
	}
	tld := domain[strings.LastIndex(domain, ".")+1:]
	if !containsText(allowedTLDs(input), tld) {
		return nil, fmt.Errorf("%w: domain extension is not allowed", adminapps.ErrInvalid)
	}
	operation := adminapps.Document{"domain": domain, "status": "blocked", "reason": "Cloudflare Registrar/DNS integration is not configured in this Go deployment", "nextAction": "configure_registrar_and_dns_then_retry", "retryable": false, "requestedAt": adminAppsNow(), "requestedBy": actor.ID}
	if _, err := r.updateAppDocument(ctx, actor, id, "app_api_domain_registration_requested", bson.M{"domainProvisioning": operation}, nil); err != nil {
		return nil, err
	}
	return adminapps.Document{"configured": false, "domain": domain, "registration": operation}, nil
}
func containsText(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *mongoAdminAppsRepository) LegalPages(ctx context.Context, id string) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	return legalPagesResponse(id, app, false), nil
}
func legalPagesResponse(id string, app adminapps.Document, includePublisherControlPlane bool) adminapps.Document {
	pages := adminapps.Document{}
	stored := adminapps.FieldObject(app, "legalPages")
	legal := adminapps.FieldObject(adminapps.FieldObject(app, "nativeConfig"), "legal")
	for _, kind := range []string{"privacy", "terms", "support", "deletion", "about"} {
		slot := adminapps.FieldObject(stored, kind)
		if slot == nil {
			slot = adminapps.Document{}
		}
		currentID := adminapps.FieldString(slot, "currentVersionId")
		storedVersions := adminapps.FieldArray(slot, "versions")
		versions := make([]any, 0, len(storedVersions))
		var current any
		for index := len(storedVersions) - 1; index >= 0; index-- {
			row := legalPageVersion(storedVersions[index])
			versions = append(versions, row)
			if adminapps.FieldString(row, "versionId") == currentID {
				current = row
			}
		}
		field := kind + "Url"
		pages[kind] = adminapps.Document{"type": kind, "status": firstNonEmptyCompat(adminapps.FieldString(slot, "status"), "not_uploaded"), "publicUrl": nullable(adminapps.FieldString(slot, "publicUrl")), "externalUrl": externalLegalURL(legal, field, adminapps.FieldString(slot, "publicUrl")), "current": current, "versions": versions, "updatedAt": slot["updatedAt"], "disabledAt": slot["disabledAt"], "disabledBy": legalPageActor(slot["disabledBy"])}
	}
	publishing := adminapps.FieldObject(app, "legalPublishing")
	return adminapps.Document{"appId": id, "appName": adminapps.FieldString(app, "name"), "canonicalTypes": stringToAny([]string{"privacy", "terms", "support", "deletion", "about"}), "aliases": adminapps.Document{"private": "privacy", "users": "terms"}, "publishing": adminapps.Document{"approvedBrandDomain": nullable(adminapps.FieldString(publishing, "approvedBrandDomain")), "publisherBinding": legalPublisherBinding(adminapps.FieldObject(publishing, "publisherBinding"), includePublisherControlPlane)}, "pages": pages}
}

func legalPageVersion(value any) adminapps.Document {
	stored := nestedDocument(value)
	version := adminapps.Document{}
	for _, field := range []string{"versionId", "publicUrl", "fileName", "contentType", "sha256", "size", "status", "uploadedAt", "warnings", "validation"} {
		version[field] = stored[field]
	}
	version["uploadedBy"] = legalPageActor(stored["uploadedBy"])
	return version
}

func legalPageActor(value any) any {
	if id := stringValue(value); id != "" {
		return adminapps.Document{"id": id, "name": id}
	}
	actor := nestedDocument(value)
	id := firstNonEmptyCompat(adminapps.FieldString(actor, "id"), adminapps.FieldString(actor, "_id"))
	if id == "" {
		return nil
	}
	return adminapps.Document{"id": id, "name": firstNonEmptyCompat(adminapps.FieldString(actor, "name"), id)}
}

func legalPublisherBinding(stored adminapps.Document, includePublisherControlPlane bool) adminapps.Document {
	binding := adminapps.Document{"status": "unconfigured", "provider": nil, "distributionId": nil, "originHostname": nil, "repository": nil, "workflowPath": nil, "environment": nil, "managedIdentityRef": nil, "approvedAt": nil, "verifiedAt": nil, "verificationMethod": nil, "updatedAt": nil}
	if stored == nil {
		return binding
	}
	binding["status"] = firstNonEmptyCompat(adminapps.FieldString(stored, "status"), "unconfigured")
	if provider := adminapps.FieldString(stored, "provider"); provider == "ai_host_managed" || includePublisherControlPlane {
		binding["provider"] = nullable(provider)
	}
	for _, field := range []string{"approvedAt", "verifiedAt", "verificationMethod", "updatedAt"} {
		binding[field] = stored[field]
	}
	if includePublisherControlPlane {
		for _, field := range []string{"distributionId", "originHostname", "repository", "workflowPath", "environment", "managedIdentityRef"} {
			binding[field] = nullable(adminapps.FieldString(stored, field))
		}
	}
	return binding
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func externalLegalURL(legal map[string]any, field, public string) any {
	value := adminapps.FieldString(legal, field)
	if value != "" && value != public {
		return value
	}
	return nil
}

func (r *mongoAdminAppsRepository) UpdateLegalPublishing(ctx context.Context, actor adminapps.Actor, id string, input map[string]any) (adminapps.Document, error) {
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	candidate, err := adminapps.BuildLegalPublishingCandidate(input, actor.ID, adminAppsNow())
	if err != nil {
		return nil, err
	}
	if adminapps.HasActiveLegalPages(app) {
		if candidate.Status == "pending" && adminapps.SameLegalPublishingPolicy(app, adminapps.Document{"legalPublishing": candidate.LegalPublishing}) {
			return legalPagesResponse(id, app, true), nil
		}
		return nil, &adminapps.ConflictError{Message: "Disable active legal pages before changing the approved publisher binding"}
	}
	if _, err := r.updateAppDocument(ctx, actor, id, "app_legal_publishing_update", bson.M{"legalPublishing": candidate.LegalPublishing}, nil); err != nil {
		return nil, err
	}
	return legalPagesResponse(id, adminapps.Merge(app, adminapps.Document{"legalPublishing": candidate.LegalPublishing}), true), nil
}
func (r *mongoAdminAppsRepository) EnableManagedLegalPublishing(ctx context.Context, actor adminapps.Actor, id string) (adminapps.Document, error) {
	_, _, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: managed legal publishing requires configured R2 storage and a verified public origin", adminapps.ErrExternalUnavailable)
}
func (r *mongoAdminAppsRepository) LegalAction(ctx context.Context, actor adminapps.Actor, id, action, kind string, payload map[string]any) (adminapps.Document, error) {
	if !adminapps.IsLegalPageType(kind) {
		return nil, fmt.Errorf("%w: invalid legal page type", adminapps.ErrInvalid)
	}
	_, app, err := r.appDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	if action != "disable" {
		return nil, fmt.Errorf("%w: legal-page %s requires configured R2 storage and a verified public origin", adminapps.ErrExternalUnavailable, action)
	}
	slot := adminapps.FieldObject(adminapps.FieldObject(app, "legalPages"), kind)
	currentVersionID := adminapps.FieldString(slot, "currentVersionId")
	if slot == nil || currentVersionID == "" || adminapps.FieldString(slot, "status") == "disabled" {
		return nil, adminapps.ErrNotFound
	}
	collection, err := r.collection()
	if err != nil {
		return nil, err
	}
	now := adminAppsNow()
	legalField := map[string]string{"privacy": "privacyUrl", "terms": "termsUrl", "support": "supportUrl", "deletion": "deletionUrl", "about": "aboutUrl"}[kind]
	filter := appIDFilter(id)
	filter["legalPages."+kind+".currentVersionId"] = currentVersionID
	update := bson.M{
		"$unset": bson.M{"nativeConfig.legal." + legalField: 1},
		"$set": bson.M{
			"updatedAt": now, "legalPages." + kind + ".status": "disabled", "legalPages." + kind + ".currentVersionId": nil,
			"legalPages." + kind + ".publicUrl": nil, "legalPages." + kind + ".updatedAt": now,
			"legalPages." + kind + ".disabledAt": now, "legalPages." + kind + ".disabledBy": actor.ID,
			"legalPages." + kind + ".lastAction": "disable", "legalPages." + kind + ".lastActionAt": now,
			"legalPages." + kind + ".lastActionBy": actor.ID, "legalPages." + kind + ".versions.$[version].status": "inactive",
		},
	}
	var updated bson.M
	err = collection.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After).SetArrayFilters([]any{bson.M{"version.versionId": currentVersionID}})).Decode(&updated)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, &adminapps.ConflictError{Message: "Legal page changed concurrently; retry disable"}
	}
	if err != nil {
		return nil, fmt.Errorf("disable legal page: %w", err)
	}
	if err := r.audit(ctx, actor, "app_legal_page_disable", id, now); err != nil {
		return nil, err
	}
	return legalPagesResponse(id, adminapps.Document(documentFromRaw(updated)), false), nil
}

func (r *mongoAdminAppsRepository) ListPlatformConfigs(ctx context.Context, includeDisabled bool) ([]adminapps.Document, error) {
	collection, err := r.platformCollection()
	if err != nil {
		return nil, err
	}
	filter := bson.M{}
	if !includeDisabled {
		filter["enabled"] = true
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "platform", Value: 1}, {Key: "clientId", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list platform configs: %w", err)
	}
	defer cursor.Close(ctx)
	result := []adminapps.Document{}
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			return nil, err
		}
		result = append(result, platformDocument(raw))
	}
	return result, cursor.Err()
}
func platformDocument(raw bson.M) adminapps.Document {
	doc := adminapps.Document(documentFromRaw(raw))
	doc["_id"] = valueString(raw["_id"])
	return materializePlatformConfig(doc)
}
func materializePlatformConfig(doc adminapps.Document) adminapps.Document {
	if doc["clientId"] == "" {
		doc["clientId"] = nil
	}
	if _, ok := doc["enabled"]; !ok {
		doc["enabled"] = true
	}
	if _, ok := doc["contentPolicy"]; !ok {
		doc["contentPolicy"] = adminapps.Document{"contentFetchMode": "all", "maxContentRating": "nsfw", "allowNSFW": true, "allowViolence": false, "minAge": 0, "moderationLevel": "strict", "blockedTags": []any{}, "blockedKeywords": []any{}}
	}
	if _, ok := doc["providerOverrides"]; !ok {
		doc["providerOverrides"] = adminapps.Document{}
	}
	if _, ok := doc["features"]; !ok {
		doc["features"] = adminapps.Document{}
	}
	if _, ok := doc["tierBoost"]; !ok {
		doc["tierBoost"] = adminapps.Document{}
	}
	if _, ok := doc["pricingOverrides"]; !ok {
		doc["pricingOverrides"] = adminapps.Document{"priceMultiplier": 1, "initialBonus": 30}
	}
	return doc
}
func (r *mongoAdminAppsRepository) GetPlatformConfig(ctx context.Context, platform, clientID string) (adminapps.Document, error) {
	if !adminapps.IsPlatformName(platform) {
		return nil, adminapps.ErrInvalid
	}
	collection, err := r.platformCollection()
	if err != nil {
		return nil, err
	}
	filter := bson.M{"platform": platform, "clientId": nullableClientID(clientID)}
	var raw bson.M
	if err := collection.FindOne(ctx, filter).Decode(&raw); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, adminapps.ErrNotFound
		}
		return nil, err
	}
	return platformDocument(raw), nil
}
func nullableClientID(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
func (r *mongoAdminAppsRepository) UpdatePlatformConfig(ctx context.Context, actor adminapps.Actor, platform, clientID string, input map[string]any) (adminapps.Document, error) {
	if !adminapps.IsPlatformName(platform) {
		return nil, adminapps.ErrInvalid
	}
	if (platform == "ios" || platform == "android") && input["reviewMode"] != nil {
		return nil, fmt.Errorf("%w: Native reviewMode must be updated through /admin/apps/:id/review-mode", adminapps.ErrInvalid)
	}
	collection, err := r.platformCollection()
	if err != nil {
		return nil, err
	}
	set := bson.M{"platform": platform, "clientId": nullableClientID(firstNonEmptyCompat(clientID, stringValue(input["clientId"]))), "updatedBy": actor.ID, "updatedAt": adminAppsNow()}
	for _, key := range []string{"displayName", "enabled", "reviewerMode", "templateCoverVariant", "clientIdentifiers", "contentPolicy", "providerOverrides", "features", "tierBoost", "pricingOverrides", "uiConfig", "siteConfig", "notes", "changeReason"} {
		if value, ok := input[key]; ok {
			set[key] = value
		}
	}
	if policy, ok := set["contentPolicy"].(map[string]any); ok {
		set["contentPolicy"] = normalizePolicy(policy)
	}
	var raw bson.M
	if err := collection.FindOneAndUpdate(ctx, bson.M{"platform": platform, "clientId": set["clientId"]}, bson.M{"$set": set, "$setOnInsert": bson.M{"enabled": true, "createdAt": adminAppsNow()}}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("update platform config: %w", err)
	}
	result := platformDocument(raw)
	if err := r.audit(ctx, actor, "platform_config_update", valueString(raw["_id"]), adminAppsNow()); err != nil {
		return nil, err
	}
	return result, nil
}
func normalizePolicy(policy map[string]any) adminapps.Document {
	mode := stringValue(policy["contentFetchMode"])
	if mode != "sfw" && mode != "nsfw" && mode != "all" {
		mode = "all"
	}
	result := adminapps.Document{}
	for k, v := range policy {
		result[k] = v
	}
	result["contentFetchMode"] = mode
	result["maxContentRating"] = map[string]string{"sfw": "sfw", "nsfw": "nsfw", "all": "nsfw"}[mode]
	result["allowNSFW"] = mode != "sfw"
	if _, ok := result["blockedTags"].([]any); !ok {
		result["blockedTags"] = []any{}
	}
	if _, ok := result["blockedKeywords"].([]any); !ok {
		result["blockedKeywords"] = []any{}
	}
	return result
}
func (r *mongoAdminAppsRepository) DeletePlatformConfig(ctx context.Context, actor adminapps.Actor, platform, clientID string) error {
	if platform == "default" {
		return fmt.Errorf("%w: Cannot delete default platform config", adminapps.ErrInvalid)
	}
	collection, err := r.platformCollection()
	if err != nil {
		return err
	}
	result, err := collection.DeleteOne(ctx, bson.M{"platform": platform, "clientId": nullableClientID(clientID)})
	if err != nil {
		return err
	}
	if result.DeletedCount != 1 {
		return adminapps.ErrNotFound
	}
	return r.audit(ctx, actor, "platform_config_delete", platform+":"+clientID, adminAppsNow())
}
func (r *mongoAdminAppsRepository) PatchPlatformConfig(ctx context.Context, actor adminapps.Actor, platform, clientID, operation string, input map[string]any) (adminapps.Document, error) {
	config, err := r.GetPlatformConfig(ctx, platform, clientID)
	if err != nil {
		return nil, err
	}
	switch operation {
	case "content-policy":
		config["contentPolicy"] = input["contentPolicy"]
	case "providers":
		kind := stringValue(input["type"])
		if !containsText([]string{"chat", "image", "video", "tts"}, kind) {
			return nil, fmt.Errorf("%w: Invalid provider type", adminapps.ErrInvalid)
		}
		providers := nestedDocument(config["providerOverrides"])
		providers[kind] = adminapps.Document{"providers": input["providers"], "defaultProvider": input["defaultProvider"]}
		config["providerOverrides"] = providers
	case "features":
		config["features"] = input["features"]
	default:
		return nil, adminapps.ErrInvalid
	}
	if reason := stringValue(input["changeReason"]); reason != "" {
		config["changeReason"] = reason
	}
	return r.UpdatePlatformConfig(ctx, actor, platform, clientID, config)
}
func (r *mongoAdminAppsRepository) ClonePlatformConfig(ctx context.Context, actor adminapps.Actor, source string, input map[string]any) (adminapps.Document, error) {
	target := stringValue(input["targetPlatform"])
	if !adminapps.IsPlatformName(target) {
		return nil, fmt.Errorf("%w: Invalid target platform", adminapps.ErrInvalid)
	}
	config, err := r.GetPlatformConfig(ctx, source, stringValue(input["sourceClientId"]))
	if err != nil {
		return nil, err
	}
	delete(config, "_id")
	delete(config, "createdAt")
	delete(config, "updatedAt")
	delete(config, "platform")
	if target == "ios" || target == "android" {
		delete(config, "reviewMode")
	}
	config["displayName"] = firstNonEmptyCompat(stringValue(config["displayName"]), source) + " (从 " + source + " 复制)"
	config["changeReason"] = firstNonEmptyCompat(stringValue(input["changeReason"]), "Cloned from "+source)
	return r.UpdatePlatformConfig(ctx, actor, target, stringValue(input["targetClientId"]), config)
}
func (r *mongoAdminAppsRepository) PlatformPreview(ctx context.Context, platform, clientID, tier string) (adminapps.Document, error) {
	config, err := r.GetPlatformConfig(ctx, platform, clientID)
	if errors.Is(err, adminapps.ErrNotFound) {
		return adminapps.Document{"platform": platform, "clientId": nullableClientID(clientID), "message": "No platform config found, using default settings", "effectiveTier": firstNonEmptyCompat(tier, "free"), "features": adminapps.Document{}, "contentPolicy": adminapps.Document{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return adminapps.Document{"platform": platform, "clientId": config["clientId"], "displayName": config["displayName"], "effectiveTier": firstNonEmptyCompat(tier, "free"), "originalTier": firstNonEmptyCompat(tier, "free"), "features": config["features"], "contentPolicy": config["contentPolicy"], "providerOverrides": config["providerOverrides"], "pricingOverrides": config["pricingOverrides"]}, nil
}

// Compile-time assertion documents that all Apps compatibility calls use the
// same Mongo repository and therefore share audit/error semantics.
var _ adminapps.Repository = (*mongoAdminAppsRepository)(nil)

// Keep imports used when this file is built without a database integration
// test; the HTTP status is intentionally part of the external-state contract.
var _ = http.StatusServiceUnavailable
var _ = sort.Strings

func firstNonEmptyCompat(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
