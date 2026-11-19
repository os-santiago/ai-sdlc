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
)

type Finding struct {
	Kind      FindingKind `json:"kind"`
	Criterion int         `json:"criterion,omitempty"` // 1-based; 0 for sections
	Section   string      `json:"section,omitempty"`
	Reason    string      `json:"reason"`
}

var deliveryObligation = regexp.MustCompile(`(?i)\b(independent (behavior )?(verification|review)|required checks|signed build|provenance|promot(e|ion)|deploy(ed|ment)?|test-production|production environment|record the deployed)\b`)

// Clauses that need a before/after comparison of the source.
var baselineComparison = regexp.MustCompile(`(?i)(byte-for-byte|every other (line|file)|all other (lines|files)|otherwise unchanged)`)

var operationalSection = regexp.MustCompile(`(?i)\b(approvals?|expir(es|y|ation)|renew(al)?|budgets?|authority|deadline)\b`)

var heading = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*$`)

// ReviewabilityFindings returns every criterion or section that the read-only
// independent reviewer cannot judge from the candidate.
func ReviewabilityFindings(issue Issue) []Finding {
	var findings []Finding
	for i, criterion := range issue.AcceptanceCriteria {
		switch {
		case deliveryObligation.MatchString(criterion):
			findings = append(findings, Finding{Kind: DeliveryObligation, Criterion: i + 1, Reason: fmt.Sprintf(
				"Acceptance criterion %d is a delivery obligation enforced by AI-SDLC policy, not a property a reviewer can verify in the candidate; remove it from the criteria.", i+1)})
		case baselineComparison.MatchString(criterion):
			findings = append(findings, Finding{Kind: BaselineComparison, Criterion: i + 1, Reason: fmt.Sprintf(
				"Acceptance criterion %d requires comparing with the original source, which the read-only reviewer does not receive; state only the files and changes allowed.", i+1)})
		}
	}
	for _, section := range sections(issue.Body) {
		if !acceptanceSection(section.title) && operationalSection.MatchString(section.text) {
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
		if m := heading.FindStringSubmatch(line); m != nil {
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

// withoutBaselineClause removes the trailing clause that needs a baseline,
// keeping the leading self-contained statement.
func withoutBaselineClause(criterion string) (string, bool) {
	for _, separator := range []string{"; ", ", "} {
		parts := strings.Split(criterion, separator)
		var kept []string
		for _, part := range parts {
			if baselineComparison.MatchString(part) {
				break
			}
			kept = append(kept, part)
		}
		if len(kept) > 0 && len(kept) < len(parts) {
			text := strings.TrimRight(strings.Join(kept, separator), " ,;.")
			if !baselineComparison.MatchString(text) && len(strings.TrimSpace(text)) >= 12 {
				return text + ".", true
			}
		}
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
