// Package nodecheck answers "why will this pod not schedule anywhere?" for the
// label half of that question: which nodeSelector or required nodeAffinity
// requirements no node in the cluster satisfies.
//
// The AND/OR structure is preserved because it decides the verdict:
// nodeSelector entries must all match, required nodeAffinity needs at least one
// term to match, and within a term every expression must match.
package nodecheck

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Requirement is one label constraint, flattened for reporting.
type Requirement struct {
	// Source is "nodeSelector" or "nodeAffinity".
	Source string
	Key    string
	Op     string // In, NotIn, Exists, DoesNotExist, Gt, Lt
	Values []string
}

func (r Requirement) String() string {
	switch r.Op {
	case "Exists", "DoesNotExist":
		return fmt.Sprintf("%s (%s)", r.Key, r.Op)
	default:
		return fmt.Sprintf("%s %s [%s]", r.Key, r.Op, strings.Join(r.Values, ", "))
	}
}

// Term is one nodeAffinity nodeSelectorTerm.
type Term struct {
	Exprs []Requirement
	// FieldKeys names matchFields entries. These select on node fields rather than
	// labels, so this package cannot evaluate them; recording them lets the report
	// say the term is indeterminate instead of quietly discarding it.
	FieldKeys []string
}

// Evaluable reports whether the term can be decided from labels alone.
func (t Term) Evaluable() bool { return len(t.FieldKeys) == 0 }

// Reqs is a pod's scheduling requirements.
type Reqs struct {
	Selector []Requirement
	Terms    []Term
}

func (r Reqs) Empty() bool { return len(r.Selector) == 0 && len(r.Terms) == 0 }

// Collect flattens a pod spec's nodeSelector and required nodeAffinity.
//
// podSpec is the unstructured "spec" map, so this package stays independent of any
// typed API and works identically against a live cluster or a dump.
func Collect(nodeSelector map[string]string, terms []Term) Reqs {
	var out Reqs
	keys := make([]string, 0, len(nodeSelector))
	for k := range nodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Selector = append(out.Selector, Requirement{
			Source: "nodeSelector",
			Key:    k,
			Op:     "In",
			Values: []string{nodeSelector[k]},
		})
	}
	// Every term is kept, including ones this package cannot evaluate. Dropping an
	// unevaluable term would narrow the OR and make a schedulable pod look stuck.
	out.Terms = append(out.Terms, terms...)
	return out
}

// Satisfies reports whether a node's labels meet one requirement.
func Satisfies(r Requirement, labels map[string]string) bool {
	val, present := labels[r.Key]
	switch r.Op {
	case "In":
		if !present {
			return false
		}
		for _, v := range r.Values {
			if v == val {
				return true
			}
		}
		return false
	case "NotIn":
		if !present {
			return true // an absent key is not in the set
		}
		for _, v := range r.Values {
			if v == val {
				return false
			}
		}
		return true
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	case "Gt", "Lt":
		if !present || len(r.Values) == 0 {
			return false
		}
		have, err1 := strconv.ParseInt(val, 10, 64)
		want, err2 := strconv.ParseInt(r.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if r.Op == "Gt" {
			return have > want
		}
		return have < want
	default:
		return false
	}
}

// Verdict is the outcome of evaluating a term or a whole pod against a node.
type Verdict int

const (
	Fail Verdict = iota
	Pass
	// Indeterminate means the answer depends on something not evaluated here,
	// such as matchFields. It must never be reported as a failure.
	Indeterminate
)

func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case Indeterminate:
		return "indeterminate"
	}
	return "fail"
}

// Check is one requirement evaluated against a node.
type Check struct {
	Req Requirement
	OK  bool
	// Detail explains a failure using the node's actual label value.
	Detail string
}

// NodeResult is a pod's full evaluation against one node.
type NodeResult struct {
	Name          string
	SelectorItems []Check
	TermResults   []TermResult
	Verdict       Verdict
}

// TermResult is one nodeAffinity term against one node.
type TermResult struct {
	Index   int
	Items   []Check
	Verdict Verdict
	// Skipped names matchFields keys that made this term indeterminate.
	Skipped []string
}

// Evaluate runs a pod's requirements against one node's labels.
func Evaluate(reqs Reqs, nodeName string, labels map[string]string) NodeResult {
	res := NodeResult{Name: nodeName}

	selOK := true
	for _, r := range reqs.Selector {
		c := check(r, labels)
		res.SelectorItems = append(res.SelectorItems, c)
		if !c.OK {
			selOK = false
		}
	}

	// nodeAffinity: OR across terms. An indeterminate term keeps the whole
	// affinity indeterminate rather than failing it.
	affVerdict := Pass
	if len(reqs.Terms) > 0 {
		affVerdict = Fail
		for i, t := range reqs.Terms {
			tr := TermResult{Index: i + 1, Skipped: t.FieldKeys}
			tr.Verdict = Pass
			for _, r := range t.Exprs {
				c := check(r, labels)
				tr.Items = append(tr.Items, c)
				if !c.OK {
					tr.Verdict = Fail
				}
			}
			if !t.Evaluable() && tr.Verdict == Pass {
				// Label expressions match, but an unevaluated matchFields could still
				// exclude this node.
				tr.Verdict = Indeterminate
			}
			res.TermResults = append(res.TermResults, tr)

			switch tr.Verdict {
			case Pass:
				affVerdict = Pass
			case Indeterminate:
				if affVerdict != Pass {
					affVerdict = Indeterminate
				}
			}
		}
	}

	switch {
	case !selOK:
		res.Verdict = Fail
	case affVerdict == Pass:
		res.Verdict = Pass
	case affVerdict == Indeterminate:
		res.Verdict = Indeterminate
	default:
		res.Verdict = Fail
	}
	return res
}

func check(r Requirement, labels map[string]string) Check {
	c := Check{Req: r, OK: Satisfies(r, labels)}
	if !c.OK {
		if got, present := labels[r.Key]; present {
			c.Detail = fmt.Sprintf("node has %q", got)
		} else {
			c.Detail = "label not set on this node"
		}
	}
	return c
}

// Unsatisfiable returns the nodeSelector requirements that no node in the set
// satisfies. These are the ones worth reporting cluster-wide: a label nothing
// carries will never be satisfied by scaling or rescheduling.
func Unsatisfiable(reqs Reqs, results []NodeResult) []Requirement {
	if len(results) == 0 {
		return nil
	}
	matched := make([]bool, len(reqs.Selector))
	for _, res := range results {
		for i, c := range res.SelectorItems {
			if i < len(matched) && c.OK {
				matched[i] = true
			}
		}
	}
	var dead []Requirement
	for i, r := range reqs.Selector {
		if !matched[i] {
			dead = append(dead, r)
		}
	}
	return dead
}

// Schedulable names the nodes that can run the pod, plus those whose verdict could
// not be decided.
func Schedulable(results []NodeResult) (pass, indeterminate []string) {
	for _, r := range results {
		switch r.Verdict {
		case Pass:
			pass = append(pass, r.Name)
		case Indeterminate:
			indeterminate = append(indeterminate, r.Name)
		}
	}
	return pass, indeterminate
}
