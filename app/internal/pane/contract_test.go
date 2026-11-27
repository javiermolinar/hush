package pane_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/javiermolinar/hush/app/internal/pane"
)

func TestPlacements(t *testing.T) {
	for _, tc := range []struct {
		caps  pane.Capabilities
		extra []pane.Placement
	}{
		{pane.Capabilities{}, nil},
		{pane.Capabilities{Left: true}, []pane.Placement{pane.Left}},
		{pane.Capabilities{Above: true}, []pane.Placement{pane.Above}},
		{pane.Capabilities{Tabs: true}, []pane.Placement{pane.Tab}},
		{pane.Capabilities{Left: true, Above: true, Tabs: true}, []pane.Placement{pane.Left, pane.Above, pane.Tab}},
	} {
		want := append([]pane.Placement{pane.Right, pane.Below}, tc.extra...)
		if got := tc.caps.Placements(); !slices.Equal(got, want) {
			t.Fatalf("%+v: placements = %v, want %v", tc.caps, got, want)
		}
		for _, placement := range []pane.Placement{pane.Right, pane.Below, pane.Left, pane.Above, pane.Tab, "", "down"} {
			req := pane.OpenRequest{Placement: placement, Dir: "/a worktree", Launch: pane.Launch{
				Mode: pane.LaunchArgv, Program: "example", Args: []string{"", "space and $literal"},
			}}
			if err := req.Validate(tc.caps); (err == nil) != slices.Contains(want, placement) {
				t.Errorf("%+v: validation for %q returned %v", tc.caps, placement, err)
			}
		}
	}
}

func TestOpenRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dir    string
		launch pane.Launch
		valid  bool
	}{
		{"explicit shell", "/work", pane.Launch{Mode: pane.LaunchShell, Text: "printf x | cat"}, true},
		{"no mode", "/work", pane.Launch{Text: "example"}, false},
		{"missing program", "/work", pane.Launch{Mode: pane.LaunchArgv}, false},
		{"mixed argv and shell", "/work", pane.Launch{Mode: pane.LaunchArgv, Program: "example", Text: "example"}, false},
		{"shell with args", "/work", pane.Launch{Mode: pane.LaunchShell, Text: "example", Args: []string{}}, false},
		{"NUL in argv", "/work", pane.Launch{Mode: pane.LaunchArgv, Program: "example", Args: []string{"bad\x00arg"}}, false},
		{"relative cwd", "work", pane.Launch{Mode: pane.LaunchArgv, Program: "example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := pane.OpenRequest{Placement: pane.Right, Dir: tc.dir, Launch: tc.launch}
			if err := req.Validate(pane.Capabilities{}); (err == nil) != tc.valid {
				t.Errorf("Validate = %v, want valid=%t", err, tc.valid)
			}
		})
	}
}

func TestReferenceScope(t *testing.T) {
	a, b := pane.NewScope(), pane.NewScope()
	ref := a.Ref("pane-1")
	if ref == b.Ref("pane-1") {
		t.Fatal("two bindings share a reference with the same local ID")
	}
	if id, ok := a.Resolve(ref); id != "pane-1" || !ok {
		t.Fatalf("issuer resolved its reference to %q, %t", id, ok)
	}
	var zero pane.Scope
	for _, tc := range []struct {
		scope pane.Scope
		ref   pane.Ref
	}{{b, ref}, {zero, ref}, {a, pane.Ref{}}, {a, a.Ref("")}, {zero, zero.Ref("pane-1")}} {
		if id, ok := tc.scope.Resolve(tc.ref); id != "" || ok {
			t.Errorf("resolved invalid or foreign reference to %q, %t", id, ok)
		}
	}
}

func TestReadRequestShape(t *testing.T) {
	ref := pane.NewScope().Ref("pane-1")
	for _, tc := range []struct {
		name string
		read bool
		ref  pane.Ref
		n    int
		ok   bool
	}{
		{"minimum", true, ref, 1, true},
		{"maximum", true, ref, pane.MaxReadLines, true},
		{"unsupported", false, ref, 1, false},
		{"no pane", true, pane.Ref{}, 1, false},
		{"zero lines", true, ref, 0, false},
		{"over limit", true, ref, pane.MaxReadLines + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := pane.ReadRequest{Pane: tc.ref, Lines: tc.n}
			if err := req.Validate(pane.Capabilities{Read: tc.read}); (err == nil) != tc.ok {
				t.Errorf("Validate = %v, want valid=%t", err, tc.ok)
			}
		})
	}
}

func TestResultOutcomes(t *testing.T) {
	ref := pane.NewScope().Ref("pane-1")
	diagnostic := errors.New("fixture")
	for _, tc := range []struct {
		result  pane.Result[pane.Ref]
		outcome pane.Outcome
		value   pane.Ref
		err     error
	}{
		{pane.Confirm(ref), pane.Confirmed, ref, nil},
		{pane.Reject[pane.Ref](diagnostic), pane.Rejected, pane.Ref{}, diagnostic},
		{pane.Uncertain[pane.Ref](diagnostic), pane.Unknown, pane.Ref{}, diagnostic},
		{pane.Result[pane.Ref]{}, pane.Unknown, pane.Ref{}, nil},
	} {
		value, ok := tc.result.Value()
		if tc.result.Outcome() != tc.outcome || value != tc.value || ok != (tc.outcome == pane.Confirmed) || tc.result.Err() != tc.err {
			t.Errorf("result = %q, %v, %t, %v; want outcome %q", tc.result.Outcome(), value, ok, tc.result.Err(), tc.outcome)
		}
	}
}
