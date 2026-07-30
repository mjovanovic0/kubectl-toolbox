package tree

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Palette carries the ANSI codes, disabled when not writing to a TTY.
type Palette struct{ on bool }

func NewPalette(color bool) Palette { return Palette{on: color} }

func (p Palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p Palette) dim(s string) string    { return p.wrap("2", s) }
func (p Palette) bold(s string) string   { return p.wrap("1", s) }
func (p Palette) red(s string) string    { return p.wrap("31;1", s) }
func (p Palette) yellow(s string) string { return p.wrap("33;1", s) }
func (p Palette) green(s string) string  { return p.wrap("32", s) }
func (p Palette) cyan(s string) string   { return p.wrap("36", s) }

func (p Palette) state(s State, text string) string {
	switch s {
	case StateMissing, StateError:
		return p.red(text)
	case StateWarn:
		return p.yellow(text)
	case StateUnknown:
		return p.yellow(text)
	case StateOK:
		return p.green(text)
	}
	return text
}

// Render writes the report as an ASCII tree followed by the problem summary.
func Render(w io.Writer, r *Report, p Palette) {
	if r.Header != "" {
		fmt.Fprintln(w, p.dim(r.Header))
		fmt.Fprintln(w)
	}
	if r.Root != nil {
		renderNode(w, r.Root, "", "", p)
	}
	renderTimeline(w, r, p)
	renderProblems(w, r, p)
}

// renderTimeline prints the dated facts in order. Seeing that a snapshot predates
// a resize, or that a restore ran before a PVC vanished, is often the answer.
func renderTimeline(w io.Writer, r *Report, p Palette) {
	if len(r.Timeline) == 0 {
		return
	}
	moments := make([]Moment, len(r.Timeline))
	copy(moments, r.Timeline)
	sort.SliceStable(moments, func(i, j int) bool { return moments[i].When < moments[j].When })

	fmt.Fprintln(w)
	fmt.Fprintln(w, p.bold("Timeline"))
	for _, m := range moments {
		fmt.Fprintf(w, "  %s  %s\n", p.dim(m.When), p.state(m.State, m.What))
	}
}

// renderNode prints one node. prefix is what precedes this node's connector;
// childPrefix is what precedes its children.
func renderNode(w io.Writer, n *Node, prefix, connector string, p Palette) {
	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteString(p.dim(connector))

	label := n.Label
	switch n.State {
	case StateMissing, StateError:
		label = p.red(label)
	case StateWarn, StateUnknown:
		label = p.yellow(label)
	default:
		label = p.bold(label)
	}
	sb.WriteString(label)

	for _, a := range n.Attrs {
		sb.WriteString("  ")
		if a.Key == "" {
			sb.WriteString(p.cyan(a.Val))
			continue
		}
		sb.WriteString(p.dim(a.Key + "="))
		sb.WriteString(p.cyan(a.Val))
	}
	if m := n.State.marker(); m != "" {
		sb.WriteString("  ")
		sb.WriteString(p.state(n.State, m))
	}
	fmt.Fprintln(w, sb.String())

	// Children get a vertical guide; notes align with the children block.
	childPrefix := prefix
	switch connector {
	case "":
		// root: no indent added
	case lastConn:
		childPrefix += "   "
	default:
		childPrefix += p.dim("│") + "  "
	}

	for _, note := range n.Notes {
		guide := "   "
		if len(n.Children) > 0 {
			guide = p.dim("│") + "  "
		}
		fmt.Fprintln(w, childPrefix+guide+p.dim(note))
	}

	for i, c := range n.Children {
		conn := midConn
		if i == len(n.Children)-1 {
			conn = lastConn
		}
		renderNode(w, c, childPrefix, conn, p)
	}
}

const (
	midConn  = "├─ "
	lastConn = "└─ "
)

func renderProblems(w io.Writer, r *Report, p Palette) {
	if len(r.Problems) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.green("No broken references found."))
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n", p.bold(fmt.Sprintf("Problems (%d)", len(r.Problems))))
	for _, pr := range r.Problems {
		sev := "warn"
		switch pr.Severity {
		case StateMissing:
			sev = "missing"
		case StateError:
			sev = "error"
		case StateUnknown:
			sev = "unresolved"
		}
		fmt.Fprintf(w, "  %s %s: %s\n", p.state(pr.Severity, "•"), p.state(pr.Severity, sev), pr.Message)
		if pr.Where != "" {
			fmt.Fprintf(w, "      at   %s\n", p.dim(pr.Where))
		}
		if pr.Hint != "" {
			fmt.Fprintf(w, "      hint %s\n", p.dim(pr.Hint))
		}
	}
}
