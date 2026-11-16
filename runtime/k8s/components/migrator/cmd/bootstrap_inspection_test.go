package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBootstrapInspectionProtocolRejectsMalformedAndPrivateErrors(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `[]`, `{"database":"ai_sdlc"}`,
		`{"database":"ai_sdlc","databaseURL":null}`,
		`{"database":"ai_sdlc","databaseURL":"invalid-private-sentinel","approve":true}`,
		`{"database":"ai_sdlc","databaseURL":"invalid-private-sentinel","database":"other"}`,
		`{"Database":"ai_sdlc","databaseURL":"invalid-private-sentinel"}`,
		`{"database":"ai_sdlc","databaseURL":"postgres://private-sentinel:secret-sentinel@[invalid"}`,
		strings.Repeat(" ", 16385),
	} {
		var output bytes.Buffer
		err := inspectBootstrap(strings.NewReader(raw), &output)
		if err == nil || (err.Error() != "initial database inspection denied" && err.Error() != "initial database inspection denied: connection-configuration") || output.Len() != 0 {
			t.Fatal("invalid input leaked data or produced a successful observation")
		}
	}
}
