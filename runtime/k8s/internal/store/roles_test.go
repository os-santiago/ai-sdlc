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

func TestFlowConsoleCredentialIsOptionalDistinctAndNeverAComponent(t *testing.T) {
	passwords := map[string]string{}
	for component := range componentTables {
		passwords[component] = strings.ReplaceAll(component, "-", "_") + strings.Repeat("x", 40)
	}
	if err := ValidateRoleCredentials(passwords); err != nil {
		t.Fatal("component credentials without the optional console", err)
	}
	passwords[FlowConsoleComponent] = "flow_console" + strings.Repeat("x", 40)
	if err := ValidateRoleCredentials(passwords); err != nil {
		t.Fatal("optional console credential rejected", err)
	}
	passwords[FlowConsoleComponent] = passwords["worker"]
	if ValidateRoleCredentials(passwords) == nil {
		t.Fatal("console shares a component credential")
	}
	passwords[FlowConsoleComponent] = "short"
	if ValidateRoleCredentials(passwords) == nil {
		t.Fatal("weak console credential accepted")
	}
	if _, err := ComponentRole(FlowConsoleComponent); err == nil {
		t.Fatal("the observer must never pass as a workflow component")
	}
	delete(passwords, "worker")
	passwords[FlowConsoleComponent] = "flow_console" + strings.Repeat("x", 40)
	if ValidateRoleCredentials(passwords) == nil {
		t.Fatal("console credential replaced a required component credential")
	}
}
