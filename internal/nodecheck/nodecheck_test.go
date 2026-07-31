package nodecheck

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-toolbox/internal/source"
	"github.com/mjovanovic0/kubectl-toolbox/internal/tree"
)

func TestSatisfiesEveryOperator(t *testing.T) {
	labels := map[string]string{"disktype": "ssd", "cores": "8", "gpu": ""}

	cases := []struct {
		name string
		req  Requirement
		want bool
	}{
		{"In match", Requirement{Key: "disktype", Op: "In", Values: []string{"ssd", "nvme"}}, true},
		{"In no match", Requirement{Key: "disktype", Op: "In", Values: []string{"hdd"}}, false},
		{"In absent key", Requirement{Key: "zone", Op: "In", Values: []string{"a"}}, false},
		// An absent key is genuinely "not in" the set.
		{"NotIn absent key", Requirement{Key: "zone", Op: "NotIn", Values: []string{"a"}}, true},
		{"NotIn present and listed", Requirement{Key: "disktype", Op: "NotIn", Values: []string{"ssd"}}, false},
		{"NotIn present not listed", Requirement{Key: "disktype", Op: "NotIn", Values: []string{"hdd"}}, true},
		// Exists is about presence, not value: an empty value still exists.
		{"Exists empty value", Requirement{Key: "gpu", Op: "Exists"}, true},
		{"Exists absent", Requirement{Key: "zone", Op: "Exists"}, false},
		{"DoesNotExist absent", Requirement{Key: "zone", Op: "DoesNotExist"}, true},
		{"DoesNotExist present", Requirement{Key: "disktype", Op: "DoesNotExist"}, false},
		{"Gt numeric", Requirement{Key: "cores", Op: "Gt", Values: []string{"4"}}, true},
		{"Gt not greater", Requirement{Key: "cores", Op: "Gt", Values: []string{"8"}}, false},
		{"Lt numeric", Requirement{Key: "cores", Op: "Lt", Values: []string{"16"}}, true},
		// Non-numeric values cannot be compared and must not pass.
		{"Gt non-numeric label", Requirement{Key: "disktype", Op: "Gt", Values: []string{"4"}}, false},
		{"Gt missing value", Requirement{Key: "cores", Op: "Gt", Values: nil}, false},
		{"unknown operator", Requirement{Key: "cores", Op: "Weird", Values: []string{"1"}}, false},
	}
	for _, c := range cases {
		if got := Satisfies(c.req, labels); got != c.want {
			t.Errorf("%s: Satisfies(%v) = %v, want %v", c.name, c.req, got, c.want)
		}
	}
}

// nodeSelector is an AND: one failure fails the node.
func TestNodeSelectorIsConjunctive(t *testing.T) {
	reqs := Collect(map[string]string{"disktype": "ssd", "zone": "eu-west-1a"}, nil)
	res := Evaluate(reqs, "n1", map[string]string{"disktype": "ssd"})
	if res.Verdict != Fail {
		t.Errorf("a node missing one nodeSelector label must fail, got %v", res.Verdict)
	}
	res = Evaluate(reqs, "n1", map[string]string{"disktype": "ssd", "zone": "eu-west-1a"})
	if res.Verdict != Pass {
		t.Errorf("a node with both labels must pass, got %v", res.Verdict)
	}
}

// nodeAffinity terms are OR-ed: one matching term is enough.
func TestNodeAffinityTermsAreDisjunctive(t *testing.T) {
	reqs := Collect(nil, []Term{
		{Exprs: []Requirement{{Key: "gpu", Op: "Exists"}}},
		{Exprs: []Requirement{{Key: "kubernetes.io/arch", Op: "In", Values: []string{"amd64"}}}},
	})
	res := Evaluate(reqs, "n1", map[string]string{"kubernetes.io/arch": "amd64"})
	if res.Verdict != Pass {
		t.Errorf("one matching term should be enough, got %v", res.Verdict)
	}
	res = Evaluate(reqs, "n2", map[string]string{"kubernetes.io/arch": "arm64"})
	if res.Verdict != Fail {
		t.Errorf("no matching term should fail, got %v", res.Verdict)
	}
}

// Within a term the expressions are AND-ed.
func TestExpressionsWithinATermAreConjunctive(t *testing.T) {
	reqs := Collect(nil, []Term{{Exprs: []Requirement{
		{Key: "gpu", Op: "Exists"},
		{Key: "kubernetes.io/arch", Op: "In", Values: []string{"amd64"}},
	}}})
	if got := Evaluate(reqs, "n", map[string]string{"gpu": "true"}).Verdict; got != Fail {
		t.Errorf("half a term must not pass, got %v", got)
	}
	if got := Evaluate(reqs, "n", map[string]string{"gpu": "true", "kubernetes.io/arch": "amd64"}).Verdict; got != Pass {
		t.Errorf("a fully matching term must pass, got %v", got)
	}
}

// The bug in the standalone plugin: a term holding only matchFields was dropped,
// which narrows the OR and can report a schedulable pod as stuck. It must instead
// be kept and reported as undecidable.
func TestMatchFieldsOnlyTermIsIndeterminateNotDropped(t *testing.T) {
	reqs := Collect(nil, []Term{{FieldKeys: []string{"metadata.name"}}})
	if len(reqs.Terms) != 1 {
		t.Fatalf("the term must be kept, got %d terms", len(reqs.Terms))
	}
	res := Evaluate(reqs, "worker-1", map[string]string{})
	if res.Verdict != Indeterminate {
		t.Errorf("a matchFields-only term cannot be judged from labels; want indeterminate, got %v", res.Verdict)
	}
}

// A term whose labels match but which also carries matchFields is not a definite
// pass: the unevaluated field selector could still exclude the node.
func TestMatchFieldsAlongsideLabelsIsIndeterminate(t *testing.T) {
	reqs := Collect(nil, []Term{{
		Exprs:     []Requirement{{Key: "kubernetes.io/arch", Op: "In", Values: []string{"amd64"}}},
		FieldKeys: []string{"metadata.name"},
	}})
	res := Evaluate(reqs, "n", map[string]string{"kubernetes.io/arch": "amd64"})
	if res.Verdict != Indeterminate {
		t.Errorf("want indeterminate, got %v", res.Verdict)
	}
	// But a failing label expression is a definite fail regardless of matchFields.
	res = Evaluate(reqs, "n", map[string]string{"kubernetes.io/arch": "arm64"})
	if res.Verdict != Fail {
		t.Errorf("a failed label expression is decisive; want fail, got %v", res.Verdict)
	}
}

// A nodeSelector label no node carries is the actionable finding.
func TestUnsatisfiableFindsClusterWideGaps(t *testing.T) {
	reqs := Collect(map[string]string{"disktype": "ssd", "zone": "eu-west-1a"}, nil)
	results := []NodeResult{
		Evaluate(reqs, "n1", map[string]string{"disktype": "ssd"}),
		Evaluate(reqs, "n2", map[string]string{"disktype": "hdd"}),
	}
	dead := Unsatisfiable(reqs, results)
	if len(dead) != 1 || dead[0].Key != "zone" {
		t.Fatalf("want zone reported as unsatisfiable, got %+v", dead)
	}
	// disktype is satisfied by n1, so it must not be reported.
	for _, d := range dead {
		if d.Key == "disktype" {
			t.Error("disktype is satisfied somewhere and must not be listed")
		}
	}
}

func newFixture(t *testing.T) *Resolver {
	t.Helper()
	d, err := source.NewDump("../../testdata/nodecheck")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return New(d, "demo")
}

func flatten(n *tree.Node) []string {
	var out []string
	n.Walk(func(x *tree.Node) {
		s := x.Label
		for _, a := range x.Attrs {
			s += " " + a.Key + "=" + a.Val
		}
		for _, note := range x.Notes {
			s += " #" + note
		}
		out = append(out, s)
	})
	return out
}

func find(t *testing.T, lines []string, substr string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, strings.Join(lines, "\n"))
	return ""
}

func TestReportOnAStuckPod(t *testing.T) {
	rep := newFixture(t).Pod(context.Background(), "stuck-pod")
	ls := flatten(rep.Root)

	// zone is on no node; disktype is on worker-1, so only zone is the gap.
	var zone, disktype bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "zone") {
			zone = true
		}
		if strings.Contains(p.Message, "disktype") {
			disktype = true
		}
	}
	if !zone {
		t.Errorf("zone should be reported as carried by no node; got %+v", rep.Problems)
	}
	if disktype {
		t.Errorf("disktype is satisfied by worker-1 and must not be reported")
	}

	// Verdicts per node, with the AND/OR structure applied.
	if l := find(t, ls, "Node/worker-1"); !strings.Contains(l, "cannot schedule") {
		t.Errorf("worker-1 lacks the zone label so it cannot schedule; got: %s", l)
	}
	if l := find(t, ls, "Node/worker-2"); !strings.Contains(l, "cannot schedule") {
		t.Errorf("worker-2 fails too; got: %s", l)
	}

	// Soft affinity must be ignored entirely.
	for _, l := range ls {
		if strings.Contains(l, "fast") {
			t.Errorf("preferredDuringScheduling must not appear: %s", l)
		}
	}
	// And the scope limits must be stated so a clean run is not over-read.
	if l := find(t, ls, "Pod/stuck-pod"); !strings.Contains(l, "taints/tolerations") {
		t.Errorf("the report must say what it does not check; got: %s", l)
	}
	if rep.WorstState() != tree.StateMissing {
		t.Errorf("an unschedulable pod should be a missing-level finding, got %v", rep.WorstState())
	}
}

func TestReportOnAPodWithNoRequirements(t *testing.T) {
	rep := newFixture(t).Pod(context.Background(), "free-pod")
	if len(rep.Problems) != 0 {
		t.Errorf("a pod with no label requirements has no findings, got %+v", rep.Problems)
	}
	if l := find(t, flatten(rep.Root), "Pod/free-pod"); !strings.Contains(l, "every node satisfies") {
		t.Errorf("expected the no-requirements note; got: %s", l)
	}
}

// The regression that matters: a matchFields-only pod must not be declared stuck.
func TestReportDoesNotClaimUnschedulableForMatchFields(t *testing.T) {
	rep := newFixture(t).Pod(context.Background(), "fields-pod")
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "no node satisfies both") {
			t.Errorf("must not claim unschedulable when the only term is unevaluated: %+v", p)
		}
	}
	ls := flatten(rep.Root)
	if l := find(t, ls, "Pod/fields-pod"); !strings.Contains(l, "undecidable") {
		t.Errorf("expected the undecidable summary; got: %s", l)
	}
	find(t, ls, "matchFields metadata.name not evaluated")
}

// An already-scheduled pod is not held up by labels, and saying so avoids a
// pointless investigation.
func TestReportNotesAnAlreadyScheduledPod(t *testing.T) {
	rep := newFixture(t).Pod(context.Background(), "scheduled-pod")
	l := find(t, flatten(rep.Root), "Pod/scheduled-pod")
	if !strings.Contains(l, "already assigned to worker-1") {
		t.Errorf("expected the assigned-node note; got: %s", l)
	}
}

func TestReportOnAMissingPod(t *testing.T) {
	rep := newFixture(t).Pod(context.Background(), "does-not-exist")
	if len(rep.Problems) == 0 {
		t.Fatal("a missing pod must be reported")
	}
	if rep.WorstState() != tree.StateMissing {
		t.Errorf("want missing, got %v", rep.WorstState())
	}
}
