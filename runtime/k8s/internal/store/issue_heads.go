package store

import (
	"context"
	"database/sql"
	"errors"
)

// AdvanceIssue serializes generations under the receiver's own database role.
// A delayed earlier receipt is acknowledged without reverting newer state.
func AdvanceIssue(ctx context.Context, tx *sql.Tx, component, repository string, issue int, generation int64, eventID string, payload []byte) (bool, error) {
	table := ""
	switch component {
	case "admission-controller":
		table = "admission_heads"
	case "orchestrator":
		table = "planning_heads"
	default:
		return false, errors.New("unsupported issue receiver")
	}
	if generation < 1 || issue < 1 || repository == "" || eventID == "" || len(payload) == 0 || len(payload) > 1048576 {
		return false, errors.New("invalid issue generation")
	}
	if err := AssertComponent(ctx, tx, component); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(repository,issue_number) VALUES($1,$2) ON CONFLICT DO NOTHING`, repository, issue); err != nil {
		return false, err
	}
	var previous int64
	var previousID string
	if err := tx.QueryRowContext(ctx, `SELECT generation,event_id FROM `+table+` WHERE repository=$1 AND issue_number=$2 FOR UPDATE`, repository, issue).Scan(&previous, &previousID); err != nil {
		return false, err
	}
	if generation < previous {
		return false, nil
	}
	if generation == previous {
		if eventID != previousID {
			return false, errors.New("conflicting issue generation")
		}
		return false, nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET generation=$1,event_id=$2,payload=$3 WHERE repository=$4 AND issue_number=$5`, generation, eventID, payload, repository, issue)
	return err == nil, err
}
