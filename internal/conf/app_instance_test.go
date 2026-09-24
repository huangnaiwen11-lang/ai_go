package conf

import "testing"

func TestLoadAppInstanceEnvironmentTrimsExplicitValues(t *testing.T) {
	values := map[string]string{
		"CLING_APP_ID":                  " production-cn ",
		"CLING_PUBLIC_ORIGIN":           " https://app.example.test ",
		"CLING_MONGO_URI":               " mongodb://db.example.test:27017/cling_production ",
		"CLING_MONGO_DATABASE":          " cling_production ",
		"R2_BUCKET_NAME":                " cling-production-public ",
		"R2_PRIVATE_BUCKET_NAME":        " cling-production-private ",
		"POLARSTAR_B2B_ACCOUNT_REF":     " account-production ",
		"POLARSTAR_B2B_TENANT_ID":       " tenant-production ",
		"POLARSTAR_B2B_CALLBACK_ORIGIN": " https://app.example.test ",
	}

	got := LoadAppInstanceEnvironment(func(name string) string { return values[name] })
	if got.AppID != "production-cn" || got.PublicOrigin != "https://app.example.test" ||
		got.MongoURI != "mongodb://db.example.test:27017/cling_production" || got.MongoDatabase != "cling_production" ||
		got.R2PublicBucket != "cling-production-public" || got.R2PrivateBucket != "cling-production-private" ||
		got.PolarStarAccountRef != "account-production" || got.PolarStarTenantID != "tenant-production" ||
		got.CallbackOrigin != "https://app.example.test" {
		t.Fatalf("LoadAppInstanceEnvironment() = %#v", got)
	}
}

func TestLoadAppInstanceEnvironmentHandlesNilGetter(t *testing.T) {
	if got := LoadAppInstanceEnvironment(nil); got != (AppInstanceEnvironment{}) {
		t.Fatalf("LoadAppInstanceEnvironment(nil) = %#v", got)
	}
}
