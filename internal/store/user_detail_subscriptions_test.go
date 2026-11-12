package store_test

import (
	"context"
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// A customer's subscriptions are found through their licences: nothing
// fills subscriptions.user_id, so asking by it found none (#70). A
// licence matched by email counts, as it does for the licence list.
func TestUserDetailListsSubscriptions(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	user := &model.User{Email: lic.Email, Name: "Detail"}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	other := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	detail, err := s.GetUserDetail(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Subscriptions) != 1 || detail.Subscriptions[0].LicenseID != lic.ID {
		t.Fatalf("subscriptions = %+v, want the one of licence %s only (not %s)", detail.Subscriptions, lic.ID, other.ID)
	}
	if detail.Subscriptions[0].Plan == nil || detail.Subscriptions[0].Plan.Product == nil {
		t.Fatal("subscription loaded without its plan and product")
	}
}
