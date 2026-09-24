package polarstarb2b

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func restoreRoute() Route {
	return Route{StepID: "step-1", Provider: "polarstar_b2b_v2", AccountRef: "account-a", ContractVersion: "b2b.job.v2", MappingVersion: "published-v1"}
}
func digestFor(p []byte) string { s := sha256.Sum256(p); return hex.EncodeToString(s[:]) }
func restoreFixture(t *testing.T) []byte {
	t.Helper()
	p, e := os.ReadFile("testdata/request_text_to_image.golden.json")
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestRestoreRequestPreservesFrozenBytes(t *testing.T) {
	p := restoreFixture(t)
	want := append([]byte(nil), p...)
	r, err := RestoreRequest(restoreRoute(), p, digestFor(p))
	if err != nil {
		t.Fatal(err)
	}
	p[0] = '!'
	got := r.Payload()
	got[0] = '!'
	if !bytes.Equal(r.Payload(), want) || r.Digest() != digestFor(want) || r.ExternalID() != "step-1" || r.Capability() != "text_to_image" || r.AccountRef() != "account-a" {
		t.Fatal("frozen identity/bytes changed")
	}
}

func TestRestoreRequestRejectsUntrustedWire(t *testing.T) {
	for _, name := range []string{"trailing", "duplicate", "unknown", "unknown-input", "external", "key", "capability", "model", "callback-policy", "result-policy", "callback-url", "missing-policy", "null-input", "wrong-case"} {
		t.Run(name, func(t *testing.T) {
			p := restoreFixture(t)
			var wire map[string]any
			if err := json.Unmarshal(p, &wire); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown":
				wire["private"] = "x"
			case "unknown-input":
				wire["input"].(map[string]any)["workflow"] = "private"
			case "external":
				wire["externalId"] = "other"
			case "key":
				wire["idempotencyKey"] = "other"
			case "capability":
				wire["capability"] = "image_edit"
			case "model":
				wire["model"] = "private-gpu"
			case "callback-policy":
				wire["callbackPolicy"] = "unbounded"
			case "result-policy":
				wire["resultUrlPolicy"] = "temporary"
			case "callback-url":
				wire["callbackUrl"] = "http://callbacks.example.com/x"
			case "missing-policy":
				delete(wire, "callbackPolicy")
			case "null-input":
				wire["input"] = nil
			case "wrong-case":
				wire["ExternalId"] = wire["externalId"]
				delete(wire, "externalId")
			}
			p, _ = json.Marshal(wire)
			if name == "trailing" {
				p = append(p, []byte(` {}`)...)
			}
			if name == "duplicate" {
				p = []byte(strings.TrimSuffix(string(p), "}") + `,"model":"ps-auto"}`)
			}
			if _, err := RestoreRequest(restoreRoute(), p, digestFor(p)); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestRestoreRequestRejectsBadDigestAndRoute(t *testing.T) {
	p := restoreFixture(t)
	if _, err := RestoreRequest(restoreRoute(), p, strings.Repeat("0", 64)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("bad digest accepted")
	}
	for _, r := range []Route{{}, func() Route { r := restoreRoute(); r.StepID = "other"; return r }(), func() Route { r := restoreRoute(); r.ContractVersion = "b2b.job.v3"; return r }()} {
		if _, err := RestoreRequest(r, p, digestFor(p)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatal("bad route accepted")
		}
	}
}
