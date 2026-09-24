// Package appinstance validates a publish target's local identity inputs. It
// only performs syntax and consistency checks; it never contacts MongoDB, R2,
// or PolarStar.
package appinstance

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Config contains the non-secret identity of an application instance.
// MongoURI may contain credentials, so validation errors never include it.
type Config struct {
	AppID               string
	PublicOrigin        string
	MongoURI            string
	MongoDatabase       string
	R2PublicBucket      string
	R2PrivateBucket     string
	PolarStarAccountRef string
	PolarStarTenantID   string
	CallbackOrigin      string
}

var nonPublicNetworks = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Validate checks that all required values are explicit and rejects obvious
// local/shared defaults. It cannot establish uniqueness across external
// systems, and has no I/O.
func Validate(config Config) error {
	config = trimConfig(config)
	if !validIdentifier(config.AppID, 63) || isLocalDefault(config.AppID) {
		return invalid("AppID")
	}
	if !validPublicHTTPSOrigin(config.PublicOrigin) {
		return invalid("PublicOrigin")
	}
	if !validDatabaseName(config.MongoDatabase) || isLocalDefault(config.MongoDatabase) || config.MongoDatabase == "cling_main" || config.MongoDatabase == "ai-host-v2-staging" {
		return invalid("MongoDatabase")
	}
	if !validMongoURIForDatabase(config.MongoURI, config.MongoDatabase) {
		return invalid("MongoURI/MongoDatabase")
	}
	if !validBucketName(config.R2PublicBucket) || config.R2PublicBucket == "ai-host" || config.R2PublicBucket == "cling-ai" {
		return invalid("R2PublicBucket")
	}
	if !validBucketName(config.R2PrivateBucket) || config.R2PrivateBucket == "cling-ai-private" {
		return invalid("R2PrivateBucket")
	}
	if config.R2PublicBucket == config.R2PrivateBucket {
		return invalid("R2 buckets")
	}
	if !validPolarStarIdentifier(config.PolarStarAccountRef) || isLocalDefault(config.PolarStarAccountRef) {
		return invalid("PolarStarAccountRef")
	}
	if !validPolarStarIdentifier(config.PolarStarTenantID) || isLocalDefault(config.PolarStarTenantID) {
		return invalid("PolarStarTenantID")
	}
	if !validPublicHTTPSOrigin(config.CallbackOrigin) {
		return invalid("CallbackOrigin")
	}
	if config.CallbackOrigin != config.PublicOrigin {
		return invalid("CallbackOrigin/PublicOrigin")
	}
	return nil
}

func trimConfig(config Config) Config {
	config.AppID = strings.TrimSpace(config.AppID)
	config.PublicOrigin = strings.TrimSpace(config.PublicOrigin)
	config.MongoURI = strings.TrimSpace(config.MongoURI)
	config.MongoDatabase = strings.TrimSpace(config.MongoDatabase)
	config.R2PublicBucket = strings.TrimSpace(config.R2PublicBucket)
	config.R2PrivateBucket = strings.TrimSpace(config.R2PrivateBucket)
	config.PolarStarAccountRef = strings.TrimSpace(config.PolarStarAccountRef)
	config.PolarStarTenantID = strings.TrimSpace(config.PolarStarTenantID)
	config.CallbackOrigin = strings.TrimSpace(config.CallbackOrigin)
	return config
}

func invalid(field string) error {
	return errors.New("invalid app instance configuration: " + field)
}

func isLocalDefault(value string) bool {
	switch strings.ToLower(value) {
	case "local", "default", "development", "dev":
		return true
	default:
		return false
	}
}

func validIdentifier(value string, max int) bool {
	if len(value) < 3 || len(value) > max || value != strings.ToLower(value) {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index > 0 && index < len(value)-1 && char == '-' {
			continue
		}
		return false
	}
	return true
}

func validDatabaseName(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func validBucketName(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.TrimSpace(value) != value || value != strings.ToLower(value) {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index > 0 && index < len(value)-1 && (char == '-' || char == '.') {
			continue
		}
		return false
	}
	return true
}

func validPolarStarIdentifier(value string) bool {
	if len(value) < 3 || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for index, char := range value {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index > 0 && (char == '.' || char == '_' || char == ':' || char == '-') {
			continue
		}
		return false
	}
	return true
}

func validMongoURIForDatabase(raw, database string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "mongodb" && parsed.Scheme != "mongodb+srv") || parsed.Host == "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Path != "/"+database || !publicMongoHosts(parsed.Host) {
		return false
	}
	return true
}

func publicMongoHosts(raw string) bool {
	for _, host := range strings.Split(raw, ",") {
		name, _, err := net.SplitHostPort(host)
		if err != nil {
			name = host
		}
		if !validPublicHost(name) {
			return false
		}
	}
	return true
}

func validPublicHTTPSOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	if rawPort := parsed.Port(); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return validPublicHost(parsed.Hostname())
}

func validPublicHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		address, ok := netip.AddrFromSlice(ip)
		if !ok {
			return false
		}
		address = address.Unmap()
		for _, prefix := range nonPublicNetworks {
			if prefix.Contains(address) {
				return false
			}
		}
		return address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range []string{"localhost", "local", "internal", "lan", "home"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	for _, char := range labels[len(labels)-1] {
		if char >= 'a' && char <= 'z' {
			return true
		}
	}
	return false
}
