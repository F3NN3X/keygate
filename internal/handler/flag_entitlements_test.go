package handler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// Both boolean spellings accepted by the admin API must stay booleans
// everywhere clients read them. In particular, a disabled flag must not
// become enabled in entitlements or a truthy string in an offline token.
// An older flag that is not "false" stays enabled (see featureEnabled).
func TestFlagEntitlementsMatchBooleans(t *testing.T) {
	f := newAPIFixture(t)
	admin := &AdminHandler{Store: f.s}
	want := map[string]bool{}
	for _, kind := range []string{"bool", "flag"} {
		for _, value := range []bool{false, true} {
			feature := fmt.Sprintf("%s_%t", kind, value)
			body := fmt.Sprintf(`{"plan_id":%q,"feature":%q,"value_type":%q,"value":%q}`,
				f.planA.ID, feature, kind, fmt.Sprint(value))
			status, out := f.call(admin.CreateEntitlement, http.MethodPost, "/admin/entitlements", body, nil)
			if status != http.StatusCreated {
				t.Fatalf("create %s: %d %v", feature, status, out)
			}
			want[feature] = value
		}
	}
	// Flags stored before v0.1.10 were not checked and always granted the
	// feature; such a value keeps doing so.
	for _, value := range []string{"yes", "1", "", "False"} {
		feature := "flag_legacy_" + value
		if err := f.s.CreateEntitlement(context.Background(), &model.Entitlement{
			PlanID: f.planA.ID, Feature: feature, ValueType: "flag", Value: value,
		}); err != nil {
			t.Fatal(err)
		}
		want[feature] = true
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ls := service.NewLicenseService(f.s, priv, slog.Default(), nil, nil)
	lh := NewLicenseHandler(ls)
	body := fmt.Sprintf(`{"license_key":%q,"identifier":"device-1"}`, f.licA.LicenseKey)

	checkFeatures := func(where string, features map[string]any) {
		t.Helper()
		for feature, expected := range want {
			got, ok := features[feature].(bool)
			if !ok || got != expected {
				t.Errorf("%s feature %s = %#v (%T), want boolean %t",
					where, feature, features[feature], features[feature], expected)
			}
		}
	}
	for _, operation := range []string{"activate", "verify"} {
		fn := lh.Activate
		if operation == "verify" {
			fn = lh.Verify
		}
		status, out := f.call(fn, http.MethodPost, "/license/"+operation, body, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %v", operation, status, out)
		}
		data := out["data"].(map[string]any)
		if operation == "verify" {
			checkFeatures("verify response", data["features"].(map[string]any))
		}
		token, err := license.Verify(data["token"].(string), pub)
		if err != nil {
			t.Fatalf("%s token: %v", operation, err)
		}
		checkFeatures(operation+" token", token.Features)
	}

	eh := NewEntitlementHandler(service.NewEntitlementService(f.s, slog.Default()))
	status, out := f.call(eh.Check, http.MethodPost, "/license/entitlements",
		fmt.Sprintf(`{"license_key":%q}`, f.licA.LicenseKey), nil)
	if status != http.StatusOK {
		t.Fatalf("entitlements: %d %v", status, out)
	}
	features := out["data"].(map[string]any)["features"].(map[string]any)
	for feature, expected := range want {
		entry, ok := features[feature].(map[string]any)
		if !ok {
			t.Errorf("entitlement %s missing from %v", feature, features)
			continue
		}
		if got, ok := entry["enabled"].(bool); !ok || got != expected {
			t.Errorf("entitlement %s enabled = %#v, want %t", feature, entry["enabled"], expected)
		}
	}
}
