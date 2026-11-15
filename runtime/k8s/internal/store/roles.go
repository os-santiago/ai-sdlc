package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

var componentTables = map[string][]string{
	"intake": {}, "admission-controller": {"admissions"},
	"orchestrator": {"plans", "work_items"}, "worker": {"attempts"},
	"verifier": {}, "release-manager": {"release_gates"},
	"relay": {}, "deployer": {},
}
var rolePasswordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

func ComponentRole(component string) (string, error) {
	if _, ok := componentTables[component]; !ok {
		return "", errors.New("unknown database component")
	}
	return "aisdlc_" + strings.ReplaceAll(component, "-", "_"), nil
}

// AssertComponent prevents a broad bootstrap/migration credential from being
// accidentally used for an operational transaction, even in local mode.
func AssertComponent(ctx context.Context, q Querier, component string) error {
	want, err := ComponentRole(component)
	if err != nil {
		return err
	}
	var actual, session string
	var elevated bool
	if err := q.QueryRowContext(ctx, `SELECT current_user, session_user,
 EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolbypassrls OR rolreplication))
 OR EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=current_user)`).Scan(&actual, &session, &elevated); err != nil {
		return errors.New("database identity unavailable")
	}
	if actual != want || session != want || elevated {
		return errors.New("database component identity mismatch")
	}
	return nil
}

// ProvisionRoles is a dedicated migrator operation for an installation-owned
// database, never a service startup operation. Passwords come from external
// Secrets and are deliberately excluded from returned errors. It is additive
// for schema ownership but reconciles each managed role's table privileges.
// The caller must not use a shared application database.
func (db *DB) ProvisionRoles(ctx context.Context, passwords map[string]string) error {
	if err := ValidateRoleCredentials(passwords); err != nil {
		return err
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421020)`); err != nil {
			return errors.New("role provisioning lock failed")
		}
		var schema string
		if err := tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
			return errors.New("database schema unavailable")
		}
		// Quote identifiers; credentials use a restricted alphabet above.
		quotedSchema := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
		rows, err := tx.QueryContext(ctx, `SELECT table_name,string_agg(quote_ident(column_name),',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema=$1 GROUP BY table_name`, schema)
		if err != nil {
			return errors.New("column privilege inventory failed")
		}
		columnRevocations := []string{}
		for rows.Next() {
			var table, columns string
			if err := rows.Scan(&table, &columns); err != nil {
				rows.Close()
				return errors.New("column privilege inventory failed")
			}
			quotedTable := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
			columnRevocations = append(columnRevocations, `REVOKE ALL (`+columns+`) ON `+quotedSchema+`.`+quotedTable+` FROM `)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return errors.New("column privilege inventory failed")
		}
		rows.Close()
		for _, revoke := range columnRevocations {
			if _, err := tx.ExecContext(ctx, revoke+"PUBLIC"); err != nil {
				return errors.New("public column privilege reconciliation failed")
			}
		}
		for component, tables := range componentTables {
			role, _ := ComponentRole(component)
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
				return errors.New("role lookup failed")
			}
			var managed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_component_roles WHERE component=$1 AND role_name=$2)`, component, role).Scan(&managed); err != nil {
				return errors.New("role ownership lookup failed")
			}
			if exists && !managed {
				return errors.New("refusing to adopt an unmanaged database role")
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, `CREATE ROLE `+role); err != nil {
					return errors.New("role creation failed")
				}
			}
			var member bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=$1)`, role).Scan(&member); err != nil {
				return errors.New("role membership lookup failed")
			}
			if member {
				return errors.New("component role has unexpected inherited or settable roles")
			}
			if _, err := tx.ExecContext(ctx, `ALTER ROLE `+role+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION PASSWORD '`+passwords[component]+`'`); err != nil {
				return errors.New("role credential reconciliation failed")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO managed_component_roles(component,role_name) VALUES($1,$2) ON CONFLICT DO NOTHING`, component, role); err != nil {
				return errors.New("role ownership registration failed")
			}
			statements := []string{
				`REVOKE ALL ON SCHEMA ` + quotedSchema + ` FROM ` + role,
				`GRANT USAGE ON SCHEMA ` + quotedSchema + ` TO ` + role,
				`REVOKE ALL ON ALL TABLES IN SCHEMA ` + quotedSchema + ` FROM ` + role,
				`GRANT SELECT ON schema_migrations TO ` + role,
			}
			if component == "relay" {
				statements = append(statements, `GRANT SELECT ON authorized_outbox TO `+role,
					`GRANT UPDATE (published_at, attempts, next_attempt_at, exhausted, last_error, lease_token, lease_until) ON authorized_outbox TO `+role)
			} else {
				statements = append(statements, `GRANT SELECT ON authorized_outbox TO `+role,
					`GRANT INSERT (producer,event_id,target,operation,receipt,payload) ON authorized_outbox TO `+role,
					`GRANT SELECT, INSERT, UPDATE ON authorized_inbox TO `+role,
					`GRANT SELECT, INSERT, DELETE ON authorized_nonces TO `+role)
			}
			for _, table := range tables {
				statements = append(statements, `GRANT SELECT, INSERT, UPDATE ON `+table+` TO `+role)
			}
			// REVOKE table privileges alone does not remove column grants.
			for _, revoke := range columnRevocations {
				if _, err := tx.ExecContext(ctx, revoke+role); err != nil {
					return errors.New("component column privilege reconciliation failed")
				}
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return errors.New("component privilege reconciliation failed")
				}
			}
		}
		// PostgreSQL installations may grant PUBLIC schema creation by default.
		if _, err := tx.ExecContext(ctx, `REVOKE CREATE ON SCHEMA `+quotedSchema+` FROM PUBLIC`); err != nil {
			return errors.New("public schema privilege reconciliation failed")
		}
		if _, err := tx.ExecContext(ctx, `REVOKE ALL ON ALL TABLES IN SCHEMA `+quotedSchema+` FROM PUBLIC`); err != nil {
			return errors.New("public table privilege reconciliation failed")
		}
		return nil
	})
}

// ValidateRoleCredentials must run before privileged installation effects.
func ValidateRoleCredentials(passwords map[string]string) error {
	if len(passwords) != len(componentTables) {
		return errors.New("all component database credentials required")
	}
	seenPasswords := map[string]bool{}
	for component, password := range passwords {
		if _, err := ComponentRole(component); err != nil {
			return err
		}
		if !rolePasswordPattern.MatchString(password) {
			return errors.New("component password must be a 32-128 character URL-safe value")
		}
		if seenPasswords[password] {
			return errors.New("component database credentials must be distinct")
		}
		seenPasswords[password] = true
	}
	return nil
}
