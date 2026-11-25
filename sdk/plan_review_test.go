package sdk

import "testing"

func TestPlanReviewVolumeAndState(t *testing.T) {
	v := PlanReviewView{Title: "Saved plan", Revision: 1, HeadRevision: 1, Source: "captured", CanStart: true}
	if !v.valid(1) || v.valid(2) {
		t.Fatal("invalid revision binding")
	}
	v.HeadRevision = 2
	if v.valid(1) {
		t.Fatal("stale revision offered Start")
	}
	v.CanStart = false
	v.Review = &PlanReviewRecord{ID: "4", ThreadID: "2", ReviewerThreadID: "5", Model: Model{Provider: "test", ModelID: "reviewer"}, Thinking: "off",
		State: "complete", Report: &PlanReviewReport{Summary: "Review summary", Checks: PlanReviewChecks{"addressed", "concern", "unverified"},
			Findings: []PlanReviewFinding{{Severity: "blocking", Area: "acceptance", Message: "Missing check"}}, Limitations: []string{"No files inspected"}}}
	if !v.valid(1) {
		t.Fatal("historical report should remain inspectable")
	}
	v.Source = "partial"
	if v.valid(1) {
		t.Fatal("partial source cannot claim requirements are addressed")
	}
	v.Review.Report.Checks.Requirements = "unverified"
	v.Review.Report.Findings = make([]PlanReviewFinding, 9)
	if v.valid(1) {
		t.Fatal("findings must be bounded")
	}
}
