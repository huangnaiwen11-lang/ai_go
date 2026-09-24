package runtimeapp

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeTrimsAndLowercasesSupportedRuntimeIdentifiers(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		identifier string
		want       Input
	}{
		{
			name:       "android",
			platform:   " Android ",
			identifier: " COM.Example.Client ",
			want:       Input{Platform: PlatformAndroid, Identifier: "com.example.client"},
		},
		{
			name:       "ios",
			platform:   " IOS ",
			identifier: " Com.Example.IOS ",
			want:       Input{Platform: PlatformIOS, Identifier: "com.example.ios"},
		},
		{
			name:       "web",
			platform:   " web ",
			identifier: " Client.Example.Test ",
			want:       Input{Platform: PlatformWeb, Identifier: "client.example.test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.platform, tt.identifier)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Normalize() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNormalizeRejectsInputsThatCannotResolveAnApp(t *testing.T) {
	for _, tt := range []struct {
		name       string
		platform   string
		identifier string
	}{
		{name: "unknown platform", platform: "desktop", identifier: "com.example.app"},
		{name: "empty identifier", platform: "android", identifier: " \t "},
		{name: "overlong identifier", platform: "web", identifier: strings.Repeat("a", maxIdentifierLength+1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(tt.platform, tt.identifier)
			if !errors.Is(err, ErrUnresolved) {
				t.Fatalf("Normalize() error = %v, want ErrUnresolved", err)
			}
		})
	}
}
