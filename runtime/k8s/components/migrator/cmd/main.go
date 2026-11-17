// Command migrator owns schema/role setup. It is a short-lived installation
// Job; no delivery service receives its bootstrap database credential.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func run() error {
	if len(os.Args) < 2 {
		return errors.New("select migrate or bootstrap-identities")
	}
	switch os.Args[1] {
	case "hold-reviewer-upgrade-fence":
		if len(os.Args) != 2 {
			return errors.New("unexpected reviewer upgrade fence arguments")
		}
		return holdReviewerUpgradeFence(os.Stdin, os.Stdout)
	case "hold-upgrade-fence":
		if len(os.Args) != 2 {
			return errors.New("unexpected upgrade fence arguments")
		}
		return holdUpgradeFence(os.Stdin, os.Stdout)
	case "describe-migrations":
		if len(os.Args) != 2 {
			return errors.New("unexpected migration catalog arguments")
		}
		catalog, err := store.MigrationCatalog()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(catalog)
	case "inspect-upgrade-migrations":
		if len(os.Args) != 2 {
			return errors.New("unexpected upgrade observation arguments")
		}
		return inspectUpgradeMigrations(os.Stdin, os.Stdout)
	case "describe-release-specs":
		if len(os.Args) != 2 {
			return errors.New("unexpected release observation arguments")
		}
		return describeReleaseSpecs(os.Stdin, os.Stdout)
	case "compile-release-installation":
		if len(os.Args) != 2 {
			return errors.New("unexpected release installation arguments")
		}
		return compileReleaseInstallation(os.Stdin, os.Stdout)
	case "create-registry-custody":
		if len(os.Args) != 2 {
			return errors.New("unexpected registry bootstrap arguments")
		}
		return createRegistryCustody(os.Stdin, os.Stdout)
	case "inspect-empty-bootstrap":
		if len(os.Args) != 2 {
			return errors.New("unexpected bootstrap inspection arguments")
		}
		return inspectBootstrap(os.Stdin, os.Stdout)
	case "compile-provenance-installation", "prepare-provenance-registry-custody":
		if len(os.Args) != 2 {
			return errors.New("unexpected provenance installation arguments")
		}
		if os.Args[1] == "compile-provenance-installation" {
			return compileProvenanceInstallation(os.Stdin, os.Stdout)
		}
		return prepareProvenanceRegistryCustody(os.Stdin, os.Stdout)
	case "compile-build-installation":
		if len(os.Args) != 2 {
			return errors.New("unexpected build configuration arguments")
		}
		return compileBuildInstallation(os.Stdin, os.Stdout)
	case "observe-build-admission", "observe-build-trust", "observe-build-profile":
		if len(os.Args) != 2 {
			return errors.New("unexpected build observation arguments")
		}
		return observeBuildInstallation(os.Args[1], os.Stdin, os.Stdout)
	case "build-admission", "prepare-build-slot":
		if len(os.Args) != 2 {
			return errors.New("unexpected build installation arguments")
		}
		return buildInstallation(os.Args[1], os.Stdin, os.Stdout)
	case "validate-bootstrap":
		if len(os.Args) != 2 {
			return errors.New("unexpected bootstrap validation arguments")
		}
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 128*1024+1))
		if err != nil || len(raw) > 128*1024 {
			return errors.New("invalid bootstrap material size")
		}
		var material authority.BootstrapMaterial
		if err := authority.DecodeStrict(raw, &material); err != nil {
			return err
		}
		return material.Validate()
	case "bootstrap-identities":
		flags := flag.NewFlagSet("bootstrap-identities", flag.ContinueOnError)
		repository := flags.String("repository", "", "repository scope")
		environment := flags.String("environment", "", "environment scope")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return errors.New("invalid identity bootstrap arguments")
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected identity bootstrap arguments")
		}
		material, err := authority.Bootstrap(*repository, *environment)
		if err != nil {
			return err
		}
		// Private JSON protocol: installer captures stdout directly; no logging.
		return json.NewEncoder(os.Stdout).Encode(material)
	case "migrate":
		if len(os.Args) != 2 {
			return errors.New("unexpected migration arguments")
		}
		file, err := os.Open(os.Getenv("AI_SDLC_DATABASE_CREDENTIALS_PATH"))
		if err != nil {
			return errors.New("component database credentials unavailable")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 16385))
		if err != nil || len(raw) > 16384 {
			return errors.New("invalid component database credential document")
		}
		var passwords map[string]string
		if err := authority.DecodeStrict(raw, &passwords); err != nil {
			return errors.New("invalid component database credential document")
		}
		if err := store.ValidateRoleCredentials(passwords); err != nil {
			return err
		}
		if os.Getenv("DATABASE_URL") == "" {
			return errors.New("bootstrap database credential required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			return errors.New("bootstrap database unavailable")
		}
		defer db.Close()
		if err := db.Migrate(ctx); err != nil {
			return errors.New("schema migration failed")
		}
		review := os.Getenv("AI_SDLC_INDEPENDENT_REVIEW")
		if review != "" && review != "false" && review != "true" {
			return errors.New("invalid independent review option")
		}
		publication := os.Getenv("AI_SDLC_PUBLICATION")
		if publication != "" && publication != "false" && publication != "true" {
			return errors.New("invalid publication option")
		}
		merge := os.Getenv("AI_SDLC_MERGE")
		if merge != "" && merge != "false" && merge != "true" {
			return errors.New("invalid merge option")
		}
		build := os.Getenv("AI_SDLC_BUILD")
		if build != "" && build != "false" && build != "true" {
			return errors.New("invalid build option")
		}
		provenance := os.Getenv("AI_SDLC_PROVENANCE")
		if provenance != "" && provenance != "false" && provenance != "true" {
			return errors.New("invalid provenance option")
		}
		release := os.Getenv("AI_SDLC_RELEASE")
		if release != "" && release != "false" && release != "true" {
			return errors.New("invalid release option")
		}
		if err := db.ProvisionRolesForDeployment(ctx, passwords, os.Getenv("AI_SDLC_RUNTIME_MODE"), review == "true", publication == "true", merge == "true", build == "true", provenance == "true", release == "true", release == "true"); err != nil {
			return err
		}
		log.Print("schema and component database roles ready")
		return nil
	default:
		return errors.New("unknown migrator operation")
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
