// Package runtimeapp defines the narrow, read-only App identity resolver used
// at runtime. It deliberately does not expose App configuration or secrets.
package runtimeapp

import (
	"context"
	"errors"
	"strings"
)

const maxIdentifierLength = 200

var ErrUnresolved = errors.New("runtime app: unresolved")

type Platform string

const (
	PlatformAndroid Platform = "android"
	PlatformIOS     Platform = "ios"
	PlatformWeb     Platform = "web"
)

// Input is the normalized, safe lookup key for one runtime client App.
type Input struct {
	Platform   Platform
	Identifier string
}

// App is intentionally a minimal safe projection. Runtime callers do not
// receive server integrations, native configuration, or other App fields.
type App struct {
	ID         string
	Platform   Platform
	Identifier string
}

// Resolver maps one platform-scoped client identifier to exactly one active
// App. Unknown, inactive, ambiguous, and invalid inputs must return
// ErrUnresolved.
type Resolver interface {
	Resolve(context.Context, string, string) (App, error)
}

// Normalize accepts only the supported runtime platforms and their bounded,
// case-insensitive identifiers. Invalid data has one opaque failure so callers
// cannot use it to distinguish an unregistered App from a malformed request.
func Normalize(platform, identifier string) (Input, error) {
	normalizedPlatform := Platform(strings.ToLower(strings.TrimSpace(platform)))
	switch normalizedPlatform {
	case PlatformAndroid, PlatformIOS, PlatformWeb:
	default:
		return Input{}, ErrUnresolved
	}

	normalizedIdentifier := strings.ToLower(strings.TrimSpace(identifier))
	if normalizedIdentifier == "" || len(normalizedIdentifier) > maxIdentifierLength {
		return Input{}, ErrUnresolved
	}
	return Input{Platform: normalizedPlatform, Identifier: normalizedIdentifier}, nil
}
