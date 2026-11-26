package headless

import (
	"strings"
	"testing"
)

func TestParseExplicitReview(t *testing.T) {
	o, err := parse([]string{"review", "start", "--thread", "12", "--request-id", "exact", "--tracked", "src/main.go",
		"--untracked", "src/test.go", "--brief", "Inspect API", "--brief", "Check tests", "--parallel", "--prompt", "Use the two reviewers",
		"--model", "provider/model/name", "--thinking", "high", "--worker-timeout-ms", "3000"})
	if err != nil || o.command != reviewCommand || o.interactive(true, true) || o.review.launch.ThreadID != "12" ||
		o.review.launch.Mode != "parallel" || o.review.launch.RequestID != "exact" || o.review.launch.WorkerTimeoutMS != 3000 ||
		o.review.launch.Selection == nil || o.review.launch.Selection.Model.ModelID != "model/name" || len(o.review.launch.Assignments) != 2 {
		t.Fatalf("review launch: %+v, %v", o.review, err)
	}
	named, err := parse([]string{"review", "start", "--thread", "12", "--request-id", "evidence", "--tracked", "main.go",
		"--reviewer", "Reader=Review pinned bytes", "--check", "finding=Inspect the default", "--evidence", "tests:Caller excerpt=3 passed"})
	if err != nil || named.review.launch.Assessment == nil || len(named.review.launch.Assessment.Checks) != 1 ||
		named.review.launch.Assessment.Checks[0].ID != "finding" || len(named.review.launch.Assessment.Evidence) != 1 ||
		named.review.launch.Assessment.Evidence[0].Label != "Caller excerpt" || named.review.launch.Assignments[0].Name != "Reader" {
		t.Fatalf("pinned review assessment: %+v, %v", named.review.launch, err)
	}
	for _, args := range [][]string{
		{"review", "start", "--thread", "12", "--request-id", "k", "--tracked", "src/main.go"},
		{"review", "status", "--thread", "12", "--request-id", "k", "--brief", "not authority"},
		{"review", "status", "--thread", "12", "--request-id", "k", "--no-session"},
		{"review", "start", "--thread", "12", "--request-id", "k", "--tracked", "a", "--brief", "read", "--check", "missing-id"},
		{"review", "start", "--thread", "12", "--request-id", "k", "--tracked", "a", "--brief", "read", "--evidence", "tests=no label"},
		{"review", "start", "--thread", "12", "--request-id", "k", "--tracked", "a", "--brief", "read", "--reviewer", "A=read"},
		{"review", "stop", "--thread", "12", "invalid-handle"},
		{"review", "start", "--print", "--thread", "12", "--request-id", "k", "--brief", "a", "--tracked", "a"},
		{"--no-session", "--brief", "review", "this is not a native launch"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted invalid review command: %v", args)
		}
	}
	if _, err := parse([]string{"review", "stop", "--thread", "12", strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	literal, err := parse([]string{"--", "review", "start"})
	if err != nil || literal.command == reviewCommand {
		t.Fatalf("literal prompt became a review launch: %+v, %v", literal, err)
	}
}
