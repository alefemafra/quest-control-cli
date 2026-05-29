package internal

import (
	"fmt"
	"regexp"
	"strings"
)

// validateAssertionsCoverage compares the spec's structural elements (FRs,
// NFRs, API endpoints) against the assertions produced by Call 1. Returns a
// list of human-readable issues; empty slice means coverage is satisfactory.
//
// The fuzzy match is intentionally lenient: an FR is considered covered when
// at least 2 distinct content tokens (words >= 4 chars, not stopwords) appear
// in any assertion text in any category. This catches obvious omissions while
// tolerating paraphrasing.
func validateAssertionsCoverage(spec string, assertions []Assertion) []string {
	if spec == "" || len(assertions) == 0 {
		return nil
	}

	allText := assertionsCombinedText(assertions)
	allTextLower := strings.ToLower(allText)

	var issues []string

	frs := extractNumberedSection(spec, "Functional Requirements")
	for i, fr := range frs {
		if !textCovered(fr, allTextLower) {
			issues = append(issues, fmt.Sprintf(
				"FR%d ('%s') has no matching assertion. Add assertions covering this requirement (likely ui.* or data.*).",
				i+1, truncatePreview(fr, 100)))
		}
	}

	nfrs := extractNumberedSection(spec, "Non-Functional Requirements")
	if len(nfrs) == 0 {
		nfrs = extractBulletSection(spec, "Non-Functional Requirements")
	}
	for i, nfr := range nfrs {
		if !textCovered(nfr, allTextLower) {
			issues = append(issues, fmt.Sprintf(
				"NFR%d ('%s') has no matching assertion. Add a11y/perf/error/security/telemetry/auth assertions as applicable.",
				i+1, truncatePreview(nfr, 100)))
		}
	}

	endpoints := extractAPIEndpoints(spec)
	for _, ep := range endpoints {
		parts := strings.SplitN(ep, " ", 2)
		if len(parts) != 2 {
			continue
		}
		method := parts[0]
		path := parts[1]
		hasHappy, hasError := endpointCoverage(assertions, method, path)
		if !hasHappy {
			issues = append(issues, fmt.Sprintf(
				"Endpoint %s has no happy-path api.* assertion. Add one covering a 2xx success case.",
				ep))
		}
		if !hasError {
			issues = append(issues, fmt.Sprintf(
				"Endpoint %s has no error-path api.* assertion. Add one covering 4xx/5xx (validation, auth, server error).",
				ep))
		}
	}

	return issues
}

// validateFeaturesCoverage checks the Call 2 output against the assertion IDs
// produced by Call 1 plus structural quality bars (scope length, validation_refs
// presence). Returns a list of human-readable issues.
func validateFeaturesCoverage(features []Feature, knownIDs map[string][]string) []string {
	var issues []string

	if len(features) == 0 {
		issues = append(issues, "No features were emitted. Decompose the spec into at least 1 feature.")
		return issues
	}

	// Track every assertion ID seen across features.
	referenced := make(map[string]bool)
	for _, f := range features {
		for _, ref := range f.ValidationRefs {
			referenced[strings.TrimSpace(ref)] = true
		}
	}

	// Flag IDs from Call 1 that no feature picked up.
	var orphaned []string
	for _, ids := range knownIDs {
		for _, id := range ids {
			if !referenced[id] {
				orphaned = append(orphaned, id)
			}
		}
	}
	if len(orphaned) > 0 {
		// Cap output length so the retry prompt does not balloon.
		preview := orphaned
		if len(preview) > 12 {
			preview = append(append([]string{}, preview[:12]...), fmt.Sprintf("... and %d more", len(orphaned)-12))
		}
		issues = append(issues, fmt.Sprintf(
			"%d assertion ID(s) not referenced by any feature: %s. Either add a feature.validation_refs entry for each, or fold them into an existing feature whose scope covers them.",
			len(orphaned), strings.Join(preview, ", ")))
	}

	// Per-feature quality checks.
	for _, f := range features {
		fid := strings.TrimSpace(f.ID)
		if fid == "" {
			fid = f.Title
		}
		if len(f.ValidationRefs) == 0 {
			issues = append(issues, fmt.Sprintf(
				"Feature %s has 0 validation_refs. Every feature must reference >=1 assertion ID.",
				fid))
		}
		if len(f.ValidationRefs) > 6 {
			issues = append(issues, fmt.Sprintf(
				"Feature %s has %d validation_refs (max 6). Split into smaller features with 2-5 refs each so a single worker session can complete reliably.",
				fid, len(f.ValidationRefs)))
		} else if len(f.ValidationRefs) > 5 {
			issues = append(issues, fmt.Sprintf(
				"Feature %s has %d validation_refs (target 2-5). Consider splitting to improve worker success rate.",
				fid, len(f.ValidationRefs)))
		}
		scope := strings.TrimSpace(f.Scope)
		if scope == "" {
			issues = append(issues, fmt.Sprintf(
				"Feature %s has empty scope. Describe schemas, validation rules, file paths, and API calls.",
				fid))
		} else if len(scope) < 80 {
			issues = append(issues, fmt.Sprintf(
				"Feature %s scope is too short (%d chars: '%s'). Specify schemas, validation, file paths.",
				fid, len(scope), truncatePreview(scope, 60)))
		}

		description := strings.TrimSpace(f.Description)
		if description == "" {
			issues = append(issues, fmt.Sprintf(
				"Feature %s has empty description. Add intent, boundaries, and implementation notes.",
				fid))
		} else {
			if len(description) < 120 {
				issues = append(issues, fmt.Sprintf(
					"Feature %s description is too short (%d chars: '%s'). Explain intent, boundaries, and pitfalls.",
					fid, len(description), truncatePreview(description, 70)))
			}
			if looselySameText(scope, description) {
				issues = append(issues, fmt.Sprintf(
					"Feature %s description duplicates scope. Use description for rationale/boundaries, not repetition.",
					fid))
			}
		}
	}

	return issues
}

// formatCoverageIssues renders a list of issues as a bullet block suitable for
// re-injection into the retry prompt's "Previous attempt had coverage gaps"
// section.
func formatCoverageIssues(issues []string) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	for _, issue := range issues {
		b.WriteString("- ")
		b.WriteString(issue)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- internal helpers ---

var coverageStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "into": true, "onto": true, "when": true,
	"then": true, "than": true, "they": true, "them": true, "their": true,
	"there": true, "have": true, "been": true, "must": true, "will": true,
	"shall": true, "user": true, "users": true, "page": true, "view": true,
	"each": true, "some": true, "only": true, "also": true, "after": true,
	"before": true, "while": true, "where": true, "which": true, "what": true,
	"about": true, "above": true, "below": true, "over": true, "under": true,
	"every": true, "single": true, "shows": true, "show": true, "make": true,
	"makes": true, "uses": true, "used": true, "using": true,
}

var coverageTokenRe = regexp.MustCompile(`[a-z0-9_/-]+`)

// textCovered reports whether at least 2 distinct content tokens from src
// appear in haystack (already lowercased). Tokens shorter than 4 chars or in
// the stopword list are ignored.
func textCovered(src, haystackLower string) bool {
	tokens := coverageTokenRe.FindAllString(strings.ToLower(src), -1)
	seen := make(map[string]bool)
	hits := 0
	for _, t := range tokens {
		if len(t) < 4 || coverageStopwords[t] {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		if strings.Contains(haystackLower, t) {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

// assertionsCombinedText concatenates every assertion item into a single
// string so token searches are O(N) per FR rather than O(N*M).
func assertionsCombinedText(assertions []Assertion) string {
	var b strings.Builder
	for _, a := range assertions {
		for _, item := range a.Items {
			b.WriteString(item)
			b.WriteString("\n")
		}
	}
	return b.String()
}

var collapseWhitespaceRe = regexp.MustCompile(`\s+`)
var stripPunctuationRe = regexp.MustCompile(`[^a-z0-9\s]+`)

func looselySameText(a, b string) bool {
	normalize := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = stripPunctuationRe.ReplaceAllString(s, " ")
		s = collapseWhitespaceRe.ReplaceAllString(s, " ")
		return strings.TrimSpace(s)
	}
	na := normalize(a)
	nb := normalize(b)
	return na != "" && nb != "" && na == nb
}

var endpointHappyRe = regexp.MustCompile(`(?i)\b(2\d{2}|201|200|204|created|success|returns|persisted)\b`)
var endpointErrorRe = regexp.MustCompile(`(?i)\b(4\d{2}|5\d{2}|400|401|403|404|409|500|error|fail|invalid|reject|missing|forbidden|unauthorized|conflict)\b`)

// endpointCoverage scans assertions for any item that mentions the endpoint
// path (or method+path) and reports whether at least one assertion text
// includes a happy-path indicator and at least one includes an error-path
// indicator.
func endpointCoverage(assertions []Assertion, method, path string) (happy bool, errorPath bool) {
	pathLower := strings.ToLower(path)
	methodLower := strings.ToLower(method)
	for _, a := range assertions {
		for _, item := range a.Items {
			itemLower := strings.ToLower(item)
			if !strings.Contains(itemLower, pathLower) {
				continue
			}
			// Path mentioned. Now check for happy/error indicators.
			// Method match boosts confidence but is not strictly required.
			_ = methodLower
			if endpointHappyRe.MatchString(itemLower) {
				happy = true
			}
			if endpointErrorRe.MatchString(itemLower) {
				errorPath = true
			}
			if happy && errorPath {
				return
			}
		}
	}
	return
}
