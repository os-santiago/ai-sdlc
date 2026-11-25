package executor

import (
	"strings"
	"testing"
	"time"
)

// SCC's exit 22 becomes the launcher's step-budget code only when the
// launcher's own clock shows that SCC's time budget cannot have ended.
func TestBudgetExitCodeSeparatesTheStepBudgetByElapsedTime(t *testing.T) {
	generator := profileFixture()
	generator.MaxSeconds = 900 // SCC receives 825 s with the default call window.
	if SCCSeconds(generator) != 825 {
		t.Fatal("fixture SCC seconds", SCCSeconds(generator))
	}
	review := profileFixture()
	review.Version = ReviewProfileVersion
	for _, tc := range []struct {
		name    string
		p       Profile
		elapsed time.Duration
		want    int
	}{
		{"early step budget", generator, 4 * time.Minute, StepBudgetExitCode},
		{"just before SCC's time budget", generator, 825*time.Second - time.Millisecond, StepBudgetExitCode},
		{"at SCC's time budget", generator, 825 * time.Second, 22},
		{"after SCC's time budget", generator, 840 * time.Second, 22},
		{"negative elapsed", generator, -time.Second, 22},
		{"reviewer", review, time.Second, 22},
	} {
		if got := budgetExitCode(tc.p, tc.elapsed); got != tc.want {
			t.Fatal(tc.name, got)
		}
	}
	// A repair shortens MaxSeconds before SCC starts; the bound follows it.
	repair := profileFixture()
	repair.MaxSeconds = 120
	if budgetExitCode(repair, 119*time.Second) != StepBudgetExitCode || budgetExitCode(repair, 120*time.Second) != 22 {
		t.Fatal("shortened budget")
	}
}

// The classification relies on SCC ending a generation with exit 22 only for
// its time or step budget. A token budget would be a third cause, so the
// launcher must never configure one.
func TestLauncherConfiguresNoTokenBudget(t *testing.T) {
	p := profileFixture()
	for _, value := range append(launchEnvironment(p, toolchainFixture(), "/workspaces"), sccArguments(p, "/workspaces/run/prompt.md", "/workspaces")...) {
		if strings.Contains(strings.ToLower(value), "total-tokens") || strings.Contains(value, "SC_MAX_TOTAL_TOKENS") {
			t.Fatal("token budget configured", value)
		}
	}
	if strings.Contains(string(SCCConfiguration(p, toolchainFixture())), "otalTokens") {
		t.Fatal("token budget in the SCC configuration")
	}
}
