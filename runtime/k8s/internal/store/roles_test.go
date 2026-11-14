package store

import (
	"context"
	"strings"
	"testing"
)

func TestSharedComponentPasswordsFailBeforeDatabaseAccess(t *testing.T) {
	passwords := map[string]string{}
	for component := range componentTables {
		passwords[component] = component + strings.Repeat("x", 40)
	}
	passwords["worker"] = passwords["intake"]
	// No pool exists: rejection must precede any SQL or credential mutation.
	db := &DB{}
	if err := db.ProvisionRoles(context.Background(), passwords); err == nil {
		t.Fatal("shared component credential accepted")
	}
}
