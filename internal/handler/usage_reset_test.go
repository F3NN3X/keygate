package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
)

// Reset zeroes the counter named by its period and period key, and no
// other; a period no counter is kept for is refused rather than
// answered "reset" for nothing.
func TestResetLicenseUsage(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	prod := &model.Product{Name: "Usage", Slug: "usage-" + f.suffix, Type: "hybrid"}
	if err := f.s.CreateProduct(f.ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Usage", Slug: "usage-plan-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 2, Active: true}
	if err := f.s.CreatePlan(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "usage-" + f.suffix + "@example.com",
		LicenseKey: "KEY-USAGE-" + f.suffix, Status: model.StatusActive}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		t.Fatal(err)
	}
	counters := []*model.UsageCounter{
		{ID: uuid.NewString(), LicenseID: lic.ID, Feature: "api_calls", Period: "daily", PeriodKey: "2026-10-27", Used: 7},
		{ID: uuid.NewString(), LicenseID: lic.ID, Feature: "api_calls", Period: "daily", PeriodKey: "2026-10-28", Used: 9},
	}
	if _, err := f.s.DB.NewInsert().Model(&counters).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	reset := func(body string) (int, map[string]any) {
		return f.call(h.ResetLicenseUsage, http.MethodPost, "/admin/licenses/"+lic.ID+"/usage/reset", body, nil, gin.Param{Key: "id", Value: lic.ID})
	}
	used := func(key string) int64 {
		var n int64
		if err := f.s.DB.NewRaw("SELECT used FROM usage_counters WHERE license_id = ? AND period_key = ?", lic.ID, key).Scan(f.ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code, out := reset(`{"feature":"api_calls","period":"fortnightly"}`); code != http.StatusBadRequest {
		t.Fatalf("reset with an unknown period = %d %v, want 400", code, out)
	}
	if code, out := reset(`{"feature":"api_calls","period":"daily","period_key":"2026-10-27"}`); code != http.StatusOK {
		t.Fatalf("reset the clicked row = %d %v, want 200", code, out)
	}
	if a, b := used("2026-10-27"), used("2026-10-28"); a != 0 || b != 9 {
		t.Fatalf("after resetting 2026-10-27: used %d and %d, want 0 and 9", a, b)
	}
}
