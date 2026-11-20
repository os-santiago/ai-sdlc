package admission

import (
	"fmt"
	"regexp"
	"strings"
)

// The independent reviewer reads only the issue text, its acceptance criteria
// and the read-only candidate tree (no Git history, no baseline, no commands)
// and must request changes on missing context or uncertainty. An issue whose
// criteria cannot be judged from that view is therefore not ready: admitting
// it predetermines a rejection. These deterministic Definition-of-Ready rules
// keep admission and review on the same contract.

// FindingKind classifies why a criterion or section is not reviewable.
type FindingKind string

const (
	// DeliveryObligation is enforced by AI-SDLC components and policy
	// (verification, review, checks, build, provenance, deployment), not a
	// property of the candidate.
	DeliveryObligation FindingKind = "delivery-obligation"
	// BaselineComparison needs the original source, which reviewers do not get.
	BaselineComparison FindingKind = "baseline-comparison"
	// OperationalSection carries authority or operations notes a reviewer
	// would read as requirements.
	OperationalSection FindingKind = "operational-section"
	// ScopeLimit restricts which files the change may touch. Verifying it
	// needs the original source, which reviewers do not get, and nothing
	// reviewable remains once the restriction is removed.
	ScopeLimit FindingKind = "scope-limit"
)

type Finding struct {
	Kind      FindingKind `json:"kind"`
	Criterion int         `json:"criterion,omitempty"` // 1-based; 0 for sections
	Section   string      `json:"section,omitempty"`
	Reason    string      `json:"reason"`
}

// Process phrasing only: product features such as "deployment status" or
// "token expiry" are reviewable properties and must not match.
var deliveryObligation = regexp.MustCompile(`(?i)(\b(obtain|pass(ing)?|require[sd]?)\b[^.;]{0,40}\bindependent (behavior )?(verification|review)\b|\bpassing required checks\b|\bsigned (build|provenance)\b|\bbuild/provenance\b|\bprovenance (verification|components?)\b|\b(promot(e|ion)|deploy(ed|ment)?)\b[^.;]{0,60}\b(to|into|through)\b[^.;]{0,40}\b(environment|production|cluster|components?)\b|\brecord the deployed\b)`)

// Change-scope restrictions name which files or paths a change may touch.
// Authorization or product rules ("users can only edit their own comments",
// "limit the label to 20 characters") must not match: the object must be the
// change itself or a file/path, never a product noun.
const scopeObject = "(`[^`]+`|\\b(files?|paths?|packages?|director(y|ies)|modules?|folders?)\\b|[\\w./-]+\\.[a-z0-9]{1,5}\\b)"

var scopeLimit = regexp.MustCompile(`(?i)(\b(limit|restrict|confine|keep|scope)\b[^.;]{0,20}\b(the|this|your)\s+(changes?|edits?|diffs?|modifications?)\b[^.;]{0,20}\b(to|within)\b` +
	`|\b(changes?|edits?|diffs?|modifications?)\s+(must|should|may)\s+(be\s+)?(confined|limited|restricted|scoped)\s+to\b` +
	`|^\s*(only\s+(modify|change|touch|edit)|(modify|change|touch|edit)\s+only)\s+(the\s+)?` + scopeObject +
	`|\b(files?|paths?)\s+other\s+than\b|\bno\s+other\s+files?\b|\bthe\s+diff\b[^.;]{0,20}\bonly\b)`)

// A scope clause that also states behavior cannot be cut without losing it.
var scopePurpose = regexp.MustCompile(`(?i)\b(so that|in order to|to make|to render|to show|to display|while|which)\b`)

// Clauses that need a before/after comparison of the source.
var baselineComparison = regexp.MustCompile(`(?i)(byte-for-byte|every other (line|file)|all other (lines|files)|otherwise unchanged)`)

var operationalTerm = regexp.MustCompile(`(?i)\b(approvals?|expir(es|y|ation)|renew(al|s)?|budgets?|authority|deadline)\b`)

var recipeMarker = regexp.MustCompile(`(?i)AI-SDLC validation recipe:`)

// Mirrors normalizedevent: headings need no space after '#'.
var heading = regexp.MustCompile(`(?m)^#{1,6}[ \t]*(\S.*?)[ \t]*$`)

// A section is operational only when it carries at least two distinct
// operations terms and never when it holds the validation recipe marker.
func operational(text string) bool {
	if recipeMarker.MatchString(text) {
		return false
	}
	seen := map[string]bool{}
	for _, m := range operationalTerm.FindAllString(text, -1) {
		seen[strings.ToLower(strings.TrimRight(m, "s"))[:4]] = true
	}
	return len(seen) >= 2
}

// ReviewabilityFindings returns every criterion or section that the read-only
// independent reviewer cannot judge from the candidate.
func ReviewabilityFindings(issue Issue) []Finding {
	var findings []Finding
	for i, criterion := range issue.AcceptanceCriteria {
		switch {
		case deliveryObligation.MatchString(criterion):
			findings = append(findings, Finding{Kind: DeliveryObligation, Criterion: i + 1, Reason: fmt.Sprintf(
				"Acceptance criterion %d is a delivery obligation enforced by AI-SDLC policy, not a property a reviewer can verify in the candidate; remove it from the criteria.", i+1)})
		case hasScopeClause(criterion):
			findings = append(findings, Finding{Kind: ScopeLimit, Criterion: i + 1, Reason: fmt.Sprintf(
				"Acceptance criterion %d limits which files the change may touch; the read-only reviewer cannot compare with the original source to verify it; remove it from the criteria.", i+1)})
		case baselineComparison.MatchString(criterion):
			findings = append(findings, Finding{Kind: BaselineComparison, Criterion: i + 1, Reason: fmt.Sprintf(
				"Acceptance criterion %d requires comparing with the original source, which the read-only reviewer does not receive; state only the required behavior.", i+1)})
		}
	}
	for _, section := range sections(issue.Body) {
		if !acceptanceSection(section.title) && operational(section.text) {
			findings = append(findings, Finding{Kind: OperationalSection, Section: section.title, Reason: fmt.Sprintf(
				"Section %q contains operational or authority notes that reviewers would treat as requirements; remove it from the issue.", section.title)})
		}
	}
	return findings
}

// ProposeRevision returns a reviewable issue body: delivery obligations and
// operational sections are removed, and baseline-comparison clauses are cut
// from their criterion when a self-contained statement remains. The software
// request and every other line are preserved. It never grants authority: the
// proposal is only a suggestion until the issue author revises the issue.
func ProposeRevision(issue Issue) (string, []Finding) {
	findings := ReviewabilityFindings(issue)
	if len(findings) == 0 {
		return issue.Body, nil
	}
	drop := map[string]bool{}
	rewrite := map[string]string{}
	var unresolved []Finding
	for _, f := range findings {
		switch f.Kind {
		case DeliveryObligation:
			drop[issue.AcceptanceCriteria[f.Criterion-1]] = true
		case ScopeLimit:
			original := issue.AcceptanceCriteria[f.Criterion-1]
			if trimmed, ok, empty := withoutScopeClause(original); empty {
				drop[original] = true
			} else if ok {
				rewrite[original] = trimmed
			} else {
				unresolved = append(unresolved, f)
			}
		case BaselineComparison:
			original := issue.AcceptanceCriteria[f.Criterion-1]
			if trimmed, ok := withoutBaselineClause(original); ok {
				rewrite[original] = trimmed
			} else {
				unresolved = append(unresolved, f)
			}
		}
	}
	var out []string
	skipping := false
	inAcceptance := false
	for _, line := range strings.Split(issue.Body, "\n") {
		if m := heading.FindStringSubmatch(line); m != nil && strings.HasPrefix(strings.TrimSpace(line), "#") {
			title := strings.TrimSpace(m[1])
			inAcceptance = acceptanceSection(title)
			skipping = false
			for _, f := range findings {
				if f.Kind == OperationalSection && f.Section == title {
					skipping = true
				}
			}
		}
		if skipping {
			continue
		}
		if inAcceptance {
			if criterion, prefix, ok := criterionLine(line); ok {
				if drop[criterion] {
					continue
				}
				if replacement, ok := rewrite[criterion]; ok {
					line = prefix + replacement
				}
			}
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n", unresolved
}

var imperative = regexp.MustCompile(`(?i)^(add|cover|display|ensure|format|keep|preserve|record|remove|render|replace|return|show|support|update|use|validate)\b`)

// withoutBaselineClause removes only the clauses that need a baseline and keeps
// every other clause. It resolves the finding only when the remainder starts
// with an imperative statement; otherwise the author must rephrase it.
func withoutBaselineClause(criterion string) (string, bool) {
	for _, separator := range []string{"; ", ", "} {
		parts := strings.Split(criterion, separator)
		var kept []string
		for _, part := range parts {
			if !baselineComparison.MatchString(part) {
				kept = append(kept, strings.TrimRight(part, " ,;."))
			}
		}
		if len(kept) == 0 || len(kept) == len(parts) {
			continue
		}
		text := strings.Join(kept, separator)
		if baselineComparison.MatchString(text) || !imperative.MatchString(strings.TrimSpace(text)) {
			return "", false
		}
		return text + ".", true
	}
	return "", false
}

type section struct{ title, text string }

func sections(body string) []section {
	var result []section
	indexes := heading.FindAllStringSubmatchIndex(body, -1)
	for i, m := range indexes {
		end := len(body)
		if i+1 < len(indexes) {
			end = indexes[i+1][0]
		}
		result = append(result, section{title: strings.TrimSpace(body[m[2]:m[3]]), text: body[m[1]:end]})
	}
	return result
}

func acceptanceSection(title string) bool {
	title = strings.TrimSpace(title)
	return strings.EqualFold(title, "acceptance criteria") || strings.EqualFold(title, "ac")
}

// Mirrors normalizedevent's acceptance bullet grammar exactly.
var criterionBullet = regexp.MustCompile(`^(\s*(?:[-*]|\d+[.)])\s*(?:\[[ xX]\]\s*)?)(.+?)\s*$`)

func criterionLine(line string) (criterion, prefix string, ok bool) {
	m := criterionBullet.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[2], m[1], true
}

func hasScopeClause(criterion string) bool {
	for _, part := range strings.Split(criterion, "; ") {
		if scopeLimit.MatchString(part) {
			return true
		}
	}
	return false
}

// withoutScopeClause removes "; "-separated clauses that only restrict scope
// (or need a baseline). empty reports that nothing reviewable remains, so the
// whole criterion can be dropped; ok reports a self-contained remainder. A
// scope clause that also states behavior is left for the author (neither).
func withoutScopeClause(criterion string) (remainder string, ok, empty bool) {
	var kept []string
	for _, part := range strings.Split(criterion, "; ") {
		part = strings.TrimRight(strings.TrimSpace(part), " ,;.")
		switch {
		case scopeLimit.MatchString(part):
			if scopePurpose.MatchString(part) {
				return "", false, false
			}
		case baselineComparison.MatchString(part):
			if trimmed, cut := withoutBaselineClause(part); cut {
				kept = append(kept, strings.TrimRight(trimmed, "."))
			} else if !strings.HasPrefix(strings.ToLower(part), "every ") && !strings.HasPrefix(strings.ToLower(part), "all other") {
				return "", false, false
			}
		default:
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return "", false, true
	}
	text := strings.Join(kept, "; ")
	if !imperative.MatchString(text) || hasScopeClause(text) || baselineComparison.MatchString(text) {
		return "", false, false
	}
	return text + ".", true, false
}
