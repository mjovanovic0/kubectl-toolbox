// Package tree holds the resolved reference graph and renders it.
package tree

// State classifies a node for display and for the problem summary.
type State int

const (
	StateOK State = iota
	StateInfo
	StateWarn
	// StateMissing is a reference that points at an object which does not exist.
	StateMissing
	StateError
	// StateUnknown is "we could not look this up", e.g. RBAC or absent CRD.
	StateUnknown
)

func (s State) marker() string {
	switch s {
	case StateMissing:
		return "✗ MISSING"
	case StateError:
		return "✗ ERROR"
	case StateWarn:
		return "! WARN"
	case StateUnknown:
		return "? UNRESOLVED"
	default:
		return ""
	}
}

// KV is one inline annotation on a node line.
type KV struct {
	Key string
	Val string
}

// ObjectRef identifies the cluster object a node stands for, so post-passes
// (such as event collection) can look it up without re-deriving it.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// Node is one object (or one logical grouping such as a volumeBackup entry).
type Node struct {
	// Label is the primary text, conventionally "Kind/name".
	Label string
	State State
	// Attrs render inline after the label as key=val.
	Attrs []KV
	// Notes render on their own indented lines under the node.
	Notes    []string
	Children []*Node
	// Ref is set when this node stands for a real object.
	Ref *ObjectRef
}

func New(label string) *Node { return &Node{Label: label} }

// Object records which cluster object this node represents.
func (n *Node) Object(kind, namespace, name, uid string) *Node {
	n.Ref = &ObjectRef{Kind: kind, Namespace: namespace, Name: name, UID: uid}
	return n
}

// Walk visits this node and all descendants.
func (n *Node) Walk(fn func(*Node)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		c.Walk(fn)
	}
}

// Unhealthy reports whether this node represents a problem worth digging into.
func (n *Node) Unhealthy() bool {
	switch n.State {
	case StateWarn, StateMissing, StateError, StateUnknown:
		return true
	}
	return false
}

func (n *Node) Attr(k, v string) *Node {
	if v != "" {
		n.Attrs = append(n.Attrs, KV{k, v})
	}
	return n
}

func (n *Node) Note(s string) *Node {
	if s != "" {
		n.Notes = append(n.Notes, s)
	}
	return n
}

// SetState assigns a state unconditionally. Use it for deliberate labelling
// (StateInfo on a heading, for example), not for flagging faults.
func (n *Node) SetState(s State) *Node {
	n.State = s
	return n
}

// Escalate raises a node's state but never lowers it, so a node that has already
// been marked an error is not quietly downgraded by a later, milder finding.
func (n *Node) Escalate(s State) *Node {
	if rank(s) > rank(n.State) {
		n.State = s
	}
	return n
}

func (n *Node) Add(child *Node) *Node {
	if child != nil {
		n.Children = append(n.Children, child)
	}
	return child
}

// Problem is an actionable finding collected during resolution. These are what
// you actually read when debugging; the tree is the evidence behind them.
type Problem struct {
	Severity State
	Where    string
	Message  string
	Hint     string
}

// Moment is one dated fact, for the chronological view. Reconstructing the order
// in which things happened is often the whole investigation.
type Moment struct {
	When  string
	What  string
	State State
}

// Report is a whole resolution result.
type Report struct {
	Header   string
	Root     *Node
	Problems []Problem
	Timeline []Moment
}

// At records a dated fact for the timeline. Empty timestamps are ignored, and
// identical facts are recorded once however many branches reach the object.
func (r *Report) At(when, what string, state State) {
	if when == "" {
		return
	}
	for _, m := range r.Timeline {
		if m.When == when && m.What == what {
			return
		}
	}
	r.Timeline = append(r.Timeline, Moment{When: when, What: what, State: state})
}

// Problem records a finding once. The same object is often reached from several
// branches, and reporting one fault repeatedly only dilutes the list.
func (r *Report) Problem(sev State, where, msg, hint string) {
	for _, p := range r.Problems {
		if p.Where == where && p.Message == msg {
			return
		}
	}
	r.Problems = append(r.Problems, Problem{Severity: sev, Where: where, Message: msg, Hint: hint})
}

// WorstState returns the most severe state anywhere in the tree, for the exit code.
func (r *Report) WorstState() State {
	worst := StateOK
	var walk func(*Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		if rank(n.State) > rank(worst) {
			worst = n.State
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(r.Root)
	for _, p := range r.Problems {
		if rank(p.Severity) > rank(worst) {
			worst = p.Severity
		}
	}
	return worst
}

func rank(s State) int {
	switch s {
	case StateOK, StateInfo:
		return 0
	case StateWarn:
		return 1
	case StateUnknown:
		return 2
	case StateMissing, StateError:
		return 3
	}
	return 0
}
