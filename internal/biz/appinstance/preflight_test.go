package appinstance

import (
	"strings"
	"testing"
)

func TestValidateAcceptsDedicatedProductionResources(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRequiresEveryInstanceIdentityField(t *testing.T) {
	for _, field := range []string{
		"AppID", "PublicOrigin", "MongoURI", "MongoDatabase", "R2PublicBucket",
		"R2PrivateBucket", "PolarStarAccountRef", "PolarStarTenantID", "CallbackOrigin",
	} {
		t.Run(field, func(t *testing.T) {
			config := validConfig()
			switch field {
			case "AppID":
				config.AppID = ""
			case "PublicOrigin":
				config.PublicOrigin = ""
			case "MongoURI":
				config.MongoURI = ""
			case "MongoDatabase":
				config.MongoDatabase = ""
			case "R2PublicBucket":
				config.R2PublicBucket = ""
			case "R2PrivateBucket":
				config.R2PrivateBucket = ""
			case "PolarStarAccountRef":
				config.PolarStarAccountRef = ""
			case "PolarStarTenantID":
				config.PolarStarTenantID = ""
			case "CallbackOrigin":
				config.CallbackOrigin = ""
			}

			err := Validate(config)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("Validate() error = %v, want missing %s", err, field)
			}
		})
	}
}

func TestValidateRejectsSharedOrLocalResources(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"local app", func(c *Config) { c.AppID = "local" }, "AppID"},
		{"default database", func(c *Config) { c.MongoDatabase = "cling_main" }, "MongoDatabase"},
		{"same buckets", func(c *Config) { c.R2PrivateBucket = c.R2PublicBucket }, "R2"},
		{"shared public bucket", func(c *Config) { c.R2PublicBucket = "cling-ai" }, "R2PublicBucket"},
		{"local polarstar tenant", func(c *Config) { c.PolarStarTenantID = "default" }, "PolarStarTenantID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.edit(&config)
			if err := Validate(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %s", err, test.want)
			}
		})
	}
}

func TestValidateRejectsUnsafeOriginsAndMongoMismatch(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"http public origin", func(c *Config) { c.PublicOrigin = "http://app.example.test" }, "PublicOrigin"},
		{"invalid public origin port", func(c *Config) {
			c.PublicOrigin, c.CallbackOrigin = "https://app.example.test:bad", "https://app.example.test:bad"
		}, "PublicOrigin"},
		{"zero public origin port", func(c *Config) {
			c.PublicOrigin, c.CallbackOrigin = "https://app.example.test:0", "https://app.example.test:0"
		}, "PublicOrigin"},
		{"loopback callback", func(c *Config) { c.CallbackOrigin = "https://127.0.0.1" }, "CallbackOrigin"},
		{"origin path", func(c *Config) { c.CallbackOrigin = "https://callbacks.example.test/hook" }, "CallbackOrigin"},
		{"callback origin differs", func(c *Config) { c.CallbackOrigin = "https://callbacks.example.test" }, "CallbackOrigin"},
		{"mongo URI database differs", func(c *Config) { c.MongoURI = "mongodb://db.example.test:27017/other?replicaSet=rs0" }, "MongoDatabase"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.edit(&config)
			if err := Validate(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %s", err, test.want)
			}
		})
	}
}

func validConfig() Config {
	return Config{
		AppID:               "cling-production-cn",
		PublicOrigin:        "https://app.example.test",
		MongoURI:            "mongodb://db.example.test:27017/cling_production?replicaSet=rs0",
		MongoDatabase:       "cling_production",
		R2PublicBucket:      "cling-production-public",
		R2PrivateBucket:     "cling-production-private",
		PolarStarAccountRef: "account-production-cn",
		PolarStarTenantID:   "tenant-production-cn",
		CallbackOrigin:      "https://app.example.test",
	}
}
