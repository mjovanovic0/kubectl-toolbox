package nodecheck

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-toolbox/internal/source"
	"github.com/mjovanovic0/kubectl-toolbox/internal/tree"
)

// Resolver evaluates a pod's label requirements against the cluster's nodes.
//
// It reads through the shared Source, so the same command works against a live
// cluster or an offline must-gather dump -- which is the point of folding this tool
// into the toolbox rather than leaving it shelling out to kubectl.
type Resolver struct {
	src source.Source
	ns  string
	rep *tree.Report
}

func New(src source.Source, namespace string) *Resolver {
	return &Resolver{src: src, ns: namespace}
}

// Pod builds the report for one pod.
func (r *Resolver) Pod(ctx context.Context, podName string) *tree.Report {
	r.rep = &tree.Report{
		Header: fmt.Sprintf("source: %s | namespace: %s", r.src.Describe(), r.ns),
	}

	pod, err := r.src.Get(ctx, source.Ref{Kind: "Pod", Namespace: r.ns, Name: podName})
	if err != nil {
		root := tree.New("Pod/" + podName).Escalate(tree.StateMissing)
		r.rep.Root = root
		r.rep.Problem(tree.StateMissing, "Pod/"+podName, err.Error(),
			"check the name and namespace; a pod deleted by a controller may have been replaced under a new name")
		return r.rep
	}

	root := tree.New("Pod/"+pod.GetName()).
		Object("Pod", pod.GetNamespace(), pod.GetName(), string(pod.GetUID()))
	r.rep.Root = root

	phase := nstr(pod.Object, "status", "phase")
	root.Attr("phase", phase)
	assigned := nstr(pod.Object, "spec", "nodeName")
	if assigned != "" {
		root.Attr("node", assigned)
	} else {
		root.Attr("node", "<unscheduled>")
	}
	// Say plainly what this tool does not consider, so a clean result is not read
	// as "nothing is wrong with scheduling".
	root.Note("evaluates label requirements only: taints/tolerations, resource requests, " +
		"topology spread and volume topology are NOT checked and can also block scheduling")

	if assigned != "" {
		root.Note("this pod is already assigned to " + assigned +
			"; label requirements are no longer what holds it up")
	}

	reqs := collectFromPod(pod)
	if reqs.Empty() {
		root.Note("no nodeSelector and no required nodeAffinity: every node satisfies its label requirements")
		return r.rep
	}
	r.addRequirements(reqs, root)

	nodes, err := r.src.List(ctx, "Node", "", "")
	if err != nil || len(nodes) == 0 {
		n := root.Add(tree.New("nodes").Escalate(tree.StateUnknown))
		if err != nil {
			n.Note("could not list nodes: " + err.Error())
		} else {
			n.Note("no nodes returned, so no verdict is possible")
		}
		r.rep.Problem(tree.StateUnknown, "Node list",
			"the cluster's nodes could not be read, so nothing can be concluded",
			"a dump must include cluster-scoped nodes for this check to work")
		return r.rep
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].GetName() < nodes[j].GetName() })

	results := make([]NodeResult, 0, len(nodes))
	for _, n := range nodes {
		results = append(results, Evaluate(reqs, n.GetName(), n.GetLabels()))
	}
	r.addNodes(results, root)
	r.summarise(reqs, results, root, podName)
	return r.rep
}

// collectFromPod reads the requirement structure out of an unstructured pod.
func collectFromPod(pod *unstructured.Unstructured) Reqs {
	sel := map[string]string{}
	if m, found, _ := unstructured.NestedStringMap(pod.Object, "spec", "nodeSelector"); found {
		sel = m
	}

	var terms []Term
	raw, _, _ := unstructured.NestedSlice(pod.Object,
		"spec", "affinity", "nodeAffinity",
		"requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	for _, item := range raw {
		tm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var t Term
		for _, e := range nslice(tm, "matchExpressions") {
			em, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			t.Exprs = append(t.Exprs, Requirement{
				Source: "nodeAffinity",
				Key:    nstr(em, "key"),
				Op:     nstr(em, "operator"),
				Values: strSlice(nslice(em, "values")),
			})
		}
		for _, f := range nslice(tm, "matchFields") {
			fm, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			t.FieldKeys = append(t.FieldKeys, nstr(fm, "key"))
		}
		// Keep the term even when it holds only matchFields, so the OR is not
		// narrowed by something this tool simply cannot evaluate.
		if len(t.Exprs) > 0 || len(t.FieldKeys) > 0 {
			terms = append(terms, t)
		}
	}
	return Collect(sel, terms)
}

func (r *Resolver) addRequirements(reqs Reqs, root *tree.Node) {
	n := root.Add(tree.New("requirements").SetState(tree.StateInfo))
	if len(reqs.Selector) > 0 {
		s := n.Add(tree.New(fmt.Sprintf("nodeSelector (%d, all must match)", len(reqs.Selector))).SetState(tree.StateInfo))
		for _, r := range reqs.Selector {
			s.Note(r.String())
		}
	}
	if len(reqs.Terms) > 0 {
		a := n.Add(tree.New(fmt.Sprintf("nodeAffinity (%d term(s), at least one must match)", len(reqs.Terms))).SetState(tree.StateInfo))
		for i, t := range reqs.Terms {
			parts := make([]string, 0, len(t.Exprs))
			for _, e := range t.Exprs {
				parts = append(parts, e.String())
			}
			line := fmt.Sprintf("term %d: %s", i+1, strings.Join(parts, " AND "))
			if len(t.FieldKeys) > 0 {
				line += fmt.Sprintf("  [+ matchFields %s, not evaluated]", strings.Join(t.FieldKeys, ","))
			}
			a.Note(line)
		}
	}
}

func (r *Resolver) addNodes(results []NodeResult, root *tree.Node) {
	group := root.Add(tree.New(fmt.Sprintf("nodes (%d)", len(results))).SetState(tree.StateInfo))
	for _, res := range results {
		n := group.Add(tree.New("Node/"+res.Name).Object("Node", "", res.Name, ""))
		switch res.Verdict {
		case Pass:
			n.Attr("verdict", "can schedule")
		case Indeterminate:
			n.Attr("verdict", "undecidable")
			n.Escalate(tree.StateWarn)
		default:
			n.Attr("verdict", "cannot schedule")
		}

		for _, c := range res.SelectorItems {
			if c.OK {
				n.Note("✓ nodeSelector " + c.Req.String())
			} else {
				n.Note("✗ nodeSelector " + c.Req.String() + "  (" + c.Detail + ")")
			}
		}
		for _, tr := range res.TermResults {
			n.Note(fmt.Sprintf("%s nodeAffinity term %d (%s)", mark(tr.Verdict), tr.Index, tr.Verdict))
			for _, c := range tr.Items {
				if c.OK {
					n.Note("    ✓ " + c.Req.String())
				} else {
					n.Note("    ✗ " + c.Req.String() + "  (" + c.Detail + ")")
				}
			}
			for _, k := range tr.Skipped {
				n.Note("    ? matchFields " + k + " not evaluated (selects on node fields, not labels)")
			}
		}
	}
}

func mark(v Verdict) string {
	switch v {
	case Pass:
		return "✓"
	case Indeterminate:
		return "?"
	}
	return "✗"
}

func (r *Resolver) summarise(reqs Reqs, results []NodeResult, root *tree.Node, podName string) {
	pass, undecided := Schedulable(results)

	root.Note(fmt.Sprintf("%d/%d node(s) satisfy the label requirements", len(pass), len(results)))
	if len(pass) > 0 {
		root.Note("can schedule on: " + strings.Join(pass, ", "))
	}
	if len(undecided) > 0 {
		root.Note("undecidable (matchFields not evaluated): " + strings.Join(undecided, ", "))
	}

	// A nodeSelector label no node carries is the most actionable finding: it will
	// never be satisfied without labelling a node.
	for _, dead := range Unsatisfiable(reqs, results) {
		r.rep.Problem(tree.StateMissing, "Pod/"+podName,
			fmt.Sprintf("no node carries the nodeSelector requirement %s", dead),
			"label a node accordingly, or correct the pod's nodeSelector; scaling the cluster will not help unless the new nodes carry this label")
	}

	if len(pass) == 0 && len(undecided) == 0 {
		root.Escalate(tree.StateMissing)
		r.rep.Problem(tree.StateMissing, "Pod/"+podName,
			"no node satisfies both nodeSelector and required nodeAffinity",
			"the per-node breakdown above shows which requirement fails on each node")
	} else if len(pass) == 0 {
		root.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, "Pod/"+podName,
			"no node definitely satisfies the requirements, and the rest depend on matchFields this tool does not evaluate",
			"inspect the matchFields terms by hand; they select on node fields such as metadata.name")
	}
}

// ---- small unstructured helpers, kept local so this package has no dependency
// on the resolve package ----

func nstr(o map[string]interface{}, fields ...string) string {
	s, _, err := unstructured.NestedString(o, fields...)
	if err != nil {
		return ""
	}
	return s
}

func nslice(o map[string]interface{}, fields ...string) []interface{} {
	s, found, err := unstructured.NestedSlice(o, fields...)
	if !found || err != nil {
		return nil
	}
	return s
}

func strSlice(raw []interface{}) []string {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
