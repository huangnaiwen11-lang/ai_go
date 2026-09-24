package adminapps

import (
	"crypto/rand"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"
)

var legalPublisherProviders = map[string]struct{}{
	"ai_host_managed": {},
	"aws_cloudfront":  {},
	"aws_s3":          {},
	"custom_nginx":    {},
	"cloudflare_r2":   {},
}

var reservedLegalPublishingTLDs = map[string]struct{}{
	"localhost": {}, "local": {}, "localdomain": {}, "internal": {}, "home": {},
	"lan": {}, "test": {}, "invalid": {}, "example": {},
}

// LegalPublishingCandidate is the validated server-owned publishing binding.
type LegalPublishingCandidate struct {
	LegalPublishing Document
	Status          string
	ApprovedDomain  string
}

// BuildLegalPublishingCandidate validates the publisher binding before it reaches Mongo.
func BuildLegalPublishingCandidate(input map[string]any, actorID string, now time.Time) (LegalPublishingCandidate, error) {
	domain, err := canonicalPublicHostname(input["approvedBrandDomain"], "approvedBrandDomain")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	raw, ok := documentValue(input["publisherBinding"])
	if !ok {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisherBinding is required", ErrInvalid)
	}
	requestedStatus, err := optionalLegalPublishingString(raw, "status")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if requestedStatus == "approved" {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisher binding cannot be approved by client input", ErrInvalid)
	}
	status := "pending"
	if requestedStatus == "revoked" {
		status = "revoked"
	}
	provider, err := requiredLegalPublishingString(raw, "provider")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if _, ok := legalPublisherProviders[provider]; !ok {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: invalid publisher provider", ErrInvalid)
	}

	distributionID, err := optionalLegalPublishingString(raw, "distributionId")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if distributionID != "" && !matchesCloudFrontDistributionID(distributionID) {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: CloudFront distributionId is invalid", ErrInvalid)
	}
	originHostname, err := optionalLegalPublishingHostname(raw, "originHostname")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	repository, err := optionalLegalPublishingString(raw, "repository")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if repository != "" && !matchesRepository(repository) {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisherBinding.repository must be owner/repo", ErrInvalid)
	}
	workflowPath, err := optionalLegalPublishingString(raw, "workflowPath")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if workflowPath != "" && !matchesWorkflowPath(workflowPath) {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisherBinding.workflowPath is invalid", ErrInvalid)
	}
	environment, err := optionalLegalPublishingString(raw, "environment")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if environment != "" && !matchesIdentifier(environment) {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisherBinding.environment is invalid", ErrInvalid)
	}
	identityRef, err := optionalLegalPublishingString(raw, "managedIdentityRef")
	if err != nil {
		return LegalPublishingCandidate{}, err
	}
	if identityRef != "" && !matchesManagedIdentityRef(identityRef) {
		return LegalPublishingCandidate{}, fmt.Errorf("%w: publisherBinding.managedIdentityRef is invalid", ErrInvalid)
	}

	binding := Document{
		"status":             status,
		"provider":           provider,
		"distributionId":     nilIfEmpty(distributionID),
		"originHostname":     nilIfEmpty(originHostname),
		"repository":         nilIfEmpty(repository),
		"workflowPath":       nilIfEmpty(workflowPath),
		"environment":        nilIfEmpty(environment),
		"managedIdentityRef": nilIfEmpty(identityRef),
		"updatedAt":          now,
		"updatedBy":          actorID,
	}
	if status == "pending" {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return LegalPublishingCandidate{}, fmt.Errorf("generate publisher verification token: %w", err)
		}
		binding["verificationToken"] = fmt.Sprintf("%x", token)
	}
	return LegalPublishingCandidate{
		LegalPublishing: Document{"approvedBrandDomain": domain, "publisherBinding": binding},
		Status:          status,
		ApprovedDomain:  domain,
	}, nil
}

// HasActiveLegalPages reports whether a current page keeps its publisher policy immutable.
func HasActiveLegalPages(app map[string]any) bool {
	pages := FieldObject(app, "legalPages")
	for _, kind := range []string{"privacy", "terms", "support", "deletion", "about"} {
		slot := FieldObject(pages, kind)
		if FieldString(slot, "currentVersionId") != "" || FieldString(slot, "publicUrl") != "" {
			return true
		}
	}
	return false
}

// SameLegalPublishingPolicy compares only the fields that lock published legal pages.
func SameLegalPublishingPolicy(left, right map[string]any) bool {
	return reflect.DeepEqual(legalPublishingPolicy(left), legalPublishingPolicy(right))
}

func legalPublishingPolicy(app map[string]any) Document {
	publishing := FieldObject(app, "legalPublishing")
	binding := FieldObject(publishing, "publisherBinding")
	policy := Document{"approvedBrandDomain": nilIfEmpty(FieldString(publishing, "approvedBrandDomain"))}
	for _, field := range []string{"provider", "originHostname", "repository", "workflowPath", "environment", "managedIdentityRef", "distributionId"} {
		policy[field] = nilIfEmpty(FieldString(binding, field))
	}
	return policy
}

func canonicalPublicHostname(value any, label string) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a canonical public hostname", ErrInvalid, label)
	}
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || strings.ContainsAny(raw, "/@:") || strings.Contains(raw, "://") || net.ParseIP(raw) != nil {
		return "", fmt.Errorf("%w: %s must be a canonical public hostname", ErrInvalid, label)
	}
	labels := strings.Split(raw, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%w: %s must be a canonical public hostname", ErrInvalid, label)
	}
	for _, part := range labels {
		if !validHostnameLabel(part) {
			return "", fmt.Errorf("%w: %s must be a canonical public hostname", ErrInvalid, label)
		}
	}
	if _, reserved := reservedLegalPublishingTLDs[labels[len(labels)-1]]; reserved {
		return "", fmt.Errorf("%w: %s must be a canonical public hostname", ErrInvalid, label)
	}
	return raw, nil
}

func documentValue(value any) (map[string]any, bool) {
	switch document := value.(type) {
	case map[string]any:
		return document, true
	case Document:
		return map[string]any(document), true
	default:
		return nil, false
	}
}

func requiredLegalPublishingString(document map[string]any, key string) (string, error) {
	value, err := optionalLegalPublishingString(document, key)
	if err != nil || value == "" {
		return "", fmt.Errorf("%w: publisherBinding.%s is required", ErrInvalid, key)
	}
	return value, nil
}

func optionalLegalPublishingString(document map[string]any, key string) (string, error) {
	value, exists := document[key]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: publisherBinding.%s must be a string", ErrInvalid, key)
	}
	return strings.TrimSpace(text), nil
}

func optionalLegalPublishingHostname(document map[string]any, key string) (string, error) {
	value, err := optionalLegalPublishingString(document, key)
	if err != nil || value == "" {
		return value, err
	}
	return canonicalPublicHostname(value, "publisherBinding."+key)
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func validHostnameLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, char := range label {
		if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func matchesCloudFrontDistributionID(value string) bool {
	if len(value) < 5 || len(value) > 64 || value[0] != 'E' {
		return false
	}
	for _, char := range value[1:] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func matchesRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validReferencePart(parts[0]) && validReferencePart(parts[1])
}

func matchesWorkflowPath(value string) bool {
	const prefix = ".github/workflows/"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	filename := strings.TrimPrefix(value, prefix)
	return validReferencePart(strings.TrimSuffix(filename, ".yaml")) && (strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml"))
}

func matchesIdentifier(value string) bool { return validReferencePart(value) }

func matchesManagedIdentityRef(value string) bool {
	switch {
	case strings.HasPrefix(value, "arn:aws"):
		return matchesAWSRoleARN(value)
	case strings.HasPrefix(value, "sso:"):
		return validIdentityPart(strings.TrimPrefix(value, "sso:"), 3, 200, true)
	case strings.HasPrefix(value, "ssh-alias:"):
		return validIdentityPart(strings.TrimPrefix(value, "ssh-alias:"), 1, 100, false)
	case strings.HasPrefix(value, "github-environment:"):
		return validIdentityPart(strings.TrimPrefix(value, "github-environment:"), 1, 100, false)
	default:
		return false
	}
}

func matchesAWSRoleARN(value string) bool {
	const marker = ":iam::"
	const rolePrefix = ":role/"
	before, accountAndRole, ok := strings.Cut(value, marker)
	if !ok || (before != "arn:aws" && !matchesAWSPartition(before)) {
		return false
	}
	account, role, ok := strings.Cut(accountAndRole, rolePrefix)
	if !ok || len(account) != 12 || !allDigits(account) || len(role) == 0 || len(role) > 128 {
		return false
	}
	for _, char := range role {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+=,.@_/-", char) {
			return false
		}
	}
	return true
}

func matchesAWSPartition(value string) bool {
	if !strings.HasPrefix(value, "arn:aws-") || len(value) <= len("arn:aws-") {
		return false
	}
	for _, char := range strings.TrimPrefix(value, "arn:aws-") {
		if char < 'a' || char > 'z' {
			return false
		}
	}
	return true
}

func validReferencePart(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char != '.' && char != '_' && char != '-' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func validIdentityPart(value string, min, max int, allowAtSlash bool) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, char := range value {
		if validReferencePart(string(char)) || (allowAtSlash && (char == '@' || char == '/')) {
			continue
		}
		return false
	}
	return true
}

func allDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
