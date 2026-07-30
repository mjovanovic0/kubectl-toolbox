// Package resolve walks the KubeVirt / CDI / CSI reference graph and turns it
// into a renderable tree plus a list of concrete problems.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmstorage/internal/source"
	"github.com/mjovanovic0/kubectl-vmstorage/internal/tree"
)

// Options tune a resolution run.
type Options struct {
	Namespace string

	// ArraySnapshotKind / ArrayVolumeKind name the vendor CRs that carry
	// storage-array detail. Defaults target Pure/Portworx CSI. The API group is
	// never assumed -- it is discovered from the Kind.
	ArraySnapshotKind string
	ArrayVolumeKind   string

	// SkipArray stops before the vendor CR lookup.
	SkipArray bool

	// NoEvents disables event collection entirely; AllEvents attaches events to
	// every resolved object rather than only to the ones already flagged.
	NoEvents  bool
	AllEvents bool

	// Timeline adds a chronological view of every dated fact found.
	Timeline bool

	// TrimRepeats collapses a sub-chain that has already been rendered in full
	// earlier in the same report, instead of repeating it. Off by default: the
	// whole point of the tool is to show the chain, and a reader following one
	// branch should not have to hunt for it elsewhere in the output.
	TrimRepeats bool

	// ShowOwners walks metadata.ownerReferences upward from the entry object to
	// reveal whatever created it (VirtualMachineClone, a Velero backup, ...).
	ShowOwners bool
}

// Resolver holds one run's state.
type Resolver struct {
	src source.Source
	opt Options
	rep *tree.Report

	// arrayCache memoises the vendor CR listings, which we scan by field match.
	arrayCache map[string][]*unstructured.Unstructured

	// rendered records objects whose sub-chain has already been written out, so
	// TrimRepeats can claim "shown elsewhere" only when that is actually true.
	rendered map[string]bool

	// events is the per-run event index, keyed by "Kind/name".
	events map[string][]event

	// classCache memoises StorageClass and VolumeSnapshotClass lookups.
	classCache        map[string]*unstructured.Unstructured
	snapClasses       []*unstructured.Unstructured
	snapClassesLoaded bool
}

// beginReport starts a fresh report and clears per-run state.
func (r *Resolver) beginReport() {
	r.rep = &tree.Report{
		Header: fmt.Sprintf("source: %s | namespace: %s", r.src.Describe(), r.opt.Namespace),
	}
	r.rendered = map[string]bool{}
	r.events = nil
	r.classCache = map[string]*unstructured.Unstructured{}
	r.snapClasses = nil
	r.snapClassesLoaded = false
}

// finish runs the post-passes that need the whole tree in hand.
func (r *Resolver) finish(ctx context.Context) *tree.Report {
	r.decorateWithEvents(ctx)
	if !r.opt.Timeline {
		r.rep.Timeline = nil
	}
	return r.rep
}

// alreadyRendered reports whether this object's sub-chain has been written out
// before, marking it as rendered when it has not. Only consulted under
// TrimRepeats, so the default path never suppresses anything.
func (r *Resolver) alreadyRendered(kind, namespace, name string) bool {
	key := kind + "/" + namespace + "/" + name
	if r.rendered[key] {
		return true
	}
	r.rendered[key] = true
	return false
}

func New(src source.Source, opt Options) *Resolver {
	if opt.ArraySnapshotKind == "" {
		opt.ArraySnapshotKind = "PureSnapshot"
	}
	if opt.ArrayVolumeKind == "" {
		opt.ArrayVolumeKind = "PureVolume"
	}
	return &Resolver{src: src, opt: opt, arrayCache: map[string][]*unstructured.Unstructured{}}
}

// getOrNode fetches ref. On success it returns the object and a bare node
// labelled Kind/name. On failure it returns a nil object and a placeholder node
// already marked Missing/Unresolved, and records a problem.
func (r *Resolver) getOrNode(ctx context.Context, ref source.Ref, why string) (*unstructured.Unstructured, *tree.Node) {
	label := ref.Kind + "/" + ref.Name
	if ref.Name == "" {
		n := tree.New(ref.Kind + "/<unset>").Escalate(tree.StateMissing)
		r.rep.Problem(tree.StateMissing, why,
			fmt.Sprintf("%s reference is empty", ref.Kind),
			"the controller has not populated this reference yet, or it failed before doing so")
		return nil, n
	}
	if ref.Group == "" {
		if g, ok := source.PreferredGroup(ref.Kind); ok {
			ref.Group = g
		}
	}
	obj, err := r.src.Get(ctx, ref)
	if err == nil {
		// Record identity so post-passes (events) can find this object again.
		return obj, tree.New(label).Object(ref.Kind, obj.GetNamespace(), obj.GetName(), string(obj.GetUID()))
	}
	switch {
	case errors.Is(err, source.ErrNotFound):
		// Even a missing object gets identity: its events often outlive it and say
		// what deleted it.
		n := tree.New(label).Object(ref.Kind, ref.Namespace, ref.Name, "").Escalate(tree.StateMissing)
		r.rep.Problem(tree.StateMissing, why,
			fmt.Sprintf("%s %q does not exist", ref.Kind, ref.Name),
			deletionHint(ref.Kind))
		return nil, n
	case errors.Is(err, source.ErrForbidden):
		n := tree.New(label).Escalate(tree.StateUnknown).Note("read forbidden")
		r.rep.Problem(tree.StateUnknown, why,
			fmt.Sprintf("not allowed to read %s %q", ref.Kind, ref.Name),
			"the chain below this point is unknown, not broken -- rerun with wider RBAC")
		return nil, n
	case errors.Is(err, source.ErrKindUnknown):
		n := tree.New(label).Escalate(tree.StateUnknown).Note(ref.Kind + " type not registered")
		r.rep.Problem(tree.StateUnknown, why,
			fmt.Sprintf("resource type %s is not present", ref.Kind),
			"the operator/CRD providing this kind is not installed, or the dump does not include it")
		return nil, n
	default:
		n := tree.New(label).Escalate(tree.StateError).Note(err.Error())
		r.rep.Problem(tree.StateError, why, err.Error(), "")
		return nil, n
	}
}

func deletionHint(kind string) string {
	switch kind {
	case "VolumeSnapshot":
		return "check whether the VolumeSnapshot was garbage-collected: a VirtualMachineSnapshot with deletionPolicy Delete removes it, and so does deleting its namespace"
	case "VolumeSnapshotContent":
		return "an unbound VolumeSnapshot has no content yet, or the content was removed while the snapshot remained"
	case "PersistentVolumeClaim":
		return "the PVC is gone; the captured spec under volumeBackups still records what it looked like, which is what you restore from"
	case "PersistentVolume":
		return "the PV was released and deleted; the array-side volume may still exist and need manual cleanup"
	case "VirtualMachineSnapshotContent":
		return "the content object backs the snapshot data; without it the snapshot cannot be restored"
	}
	return ""
}

// listKind lists a kind, tolerating absence.
func (r *Resolver) listKind(ctx context.Context, kind, namespace string) []*unstructured.Unstructured {
	group, _ := source.PreferredGroup(kind)
	objs, err := r.src.List(ctx, kind, group, namespace)
	if err != nil {
		return nil
	}
	return objs
}

// ---------- unstructured field helpers ----------

func nstr(o map[string]interface{}, fields ...string) string {
	s, _, err := unstructured.NestedString(o, fields...)
	if err != nil {
		return ""
	}
	return s
}

func nbool(o map[string]interface{}, fields ...string) (bool, bool) {
	b, found, err := unstructured.NestedBool(o, fields...)
	if err != nil {
		return false, false
	}
	return b, found
}

// boolStr renders an optional bool, empty when unset.
func boolStr(o map[string]interface{}, fields ...string) string {
	b, found := nbool(o, fields...)
	if !found {
		return ""
	}
	return strconv.FormatBool(b)
}

// nqty renders a size field that may arrive as a Quantity string ("20Gi") or as
// raw int64 bytes, which is how VolumeSnapshotContent.status.restoreSize encodes it.
func nqty(o map[string]interface{}, fields ...string) string {
	v, found, err := unstructured.NestedFieldNoCopy(o, fields...)
	if !found || err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		// A Quantity may serialise with no suffix ("171798691840"), which is valid
		// but unreadable next to sizes that do carry one. Normalise it.
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return humanBytes(n)
		}
		return t
	case int64:
		return humanBytes(t)
	case float64:
		return humanBytes(int64(t))
	}
	return ""
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(b, 10)
	}
	units := []string{"Ki", "Mi", "Gi", "Ti", "Pi"}
	v := float64(b)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10) + units[i]
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + units[i]
}

// nint reads an integer field tolerantly. The dynamic client decodes JSON numbers
// as int64, but YAML loaded from a dump arrives as float64, so a strict
// NestedInt64 silently drops values in offline mode.
func nint(o map[string]interface{}, fields ...string) (int64, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(o, fields...)
	if !found || err != nil || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case int:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func nslice(o map[string]interface{}, fields ...string) []interface{} {
	s, found, err := unstructured.NestedSlice(o, fields...)
	if !found || err != nil {
		return nil
	}
	return s
}

func nmap(o map[string]interface{}, fields ...string) map[string]interface{} {
	m, found, err := unstructured.NestedMap(o, fields...)
	if !found || err != nil {
		return nil
	}
	return m
}

func asMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

// errMessage extracts a KubeVirt/CSI style status.error.message.
func errMessage(o map[string]interface{}, path ...string) string {
	p := append(append([]string{}, path...), "message")
	if m := nstr(o, p...); m != "" {
		return m
	}
	return ""
}

// joinNonEmpty is used to build compact attribute values.
func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// sortedKeys gives map iteration a stable order so output does not shuffle
// between runs of the same command.
// timeFormat is the RFC3339 layout Kubernetes uses, so timeline entries sort
// lexically without parsing.
const timeFormat = "2006-01-02T15:04:05Z"

// createdAt renders an object's creationTimestamp, or "" when it is absent.
// Formatting a zero time would emit 0001-01-01 and pollute the timeline.
func createdAt(obj interface{ GetCreationTimestamp() metav1.Time }) string {
	t := obj.GetCreationTimestamp()
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// truncate keeps long handles readable on one line.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
