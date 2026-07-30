package resolve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// arraySpec describes how to find and display a vendor storage CR.
//
// Portworx/Pure publish the field names for PureSnapshot and PureVolume but not
// the API group, so the group is discovered from the Kind at runtime and the
// object is located by matching the CSI handle against several candidate fields
// rather than by a computed name.
type arraySpec struct {
	// match lists spec fields to compare against the handle. The pseudo-field
	// "name" means metadata.name.
	match []string
	// show lists spec fields to display when found.
	show []string
}

var arraySnapshotFields = arraySpec{
	match: []string{"name", "snapshotName", "snapshotID"},
	show:  []string{"snapshotName", "snapshotID", "podName", "policyName", "storageBackendType"},
}

var arrayVolumeFields = arraySpec{
	match: []string{"name", "volumeName", "volumeID", "serial"},
	show:  []string{"volumeName", "serial", "volumeID", "podName", "storageBackendType", "secure"},
}

// handleCandidates derives the strings an array CR might store for a CSI handle.
// Pure handles look like "<backend>::<volume>.<suffix>"; other drivers use "/"
// or no separator at all, so try the whole value and every trailing segment.
func handleCandidates(handle string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		// Trim separator debris so a prefix split cannot yield "pure-fa1:" and
		// accidentally match a field that legitimately holds "pure-fa1".
		s = strings.Trim(strings.TrimSpace(s), ":/")
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(handle)
	for _, sep := range []string{"::", ":", "/"} {
		if idx := strings.LastIndex(handle, sep); idx >= 0 {
			add(handle[idx+len(sep):])
			add(handle[:idx])
		}
	}
	return out
}

// arrayLookup parameterises one vendor-CR resolution.
type arrayLookup struct {
	kind string
	spec arraySpec
	// prefix labels the node when it is not the primary object, e.g. the volume a
	// snapshot was originally taken from.
	prefix string
	// soft downgrades a failed match to a note rather than a reported problem,
	// for lookups whose target is expected to disappear over time.
	soft bool
	// checkParent verifies that a snapshot's source volume still exists on the
	// array, which decides whether the snapshot is actually restorable.
	checkParent bool
}

// addArrayObject finds and renders the vendor CR behind a CSI handle.
func (r *Resolver) addArrayObject(ctx context.Context, parent *tree.Node, handle string, lk arrayLookup) {
	kind, spec := lk.kind, lk.spec
	if _, err := r.src.ResolveKind(kind, ""); err != nil {
		if errors.Is(err, source.ErrKindUnknown) {
			parent.Add(tree.New(lk.prefix + kind + " (not installed)").Escalate(tree.StateUnknown).
				Note("no " + kind + " resource type in this " + shortSource(r.src) + "; the handle above is still what you search for on the array"))
			return
		}
		parent.Add(tree.New(lk.prefix + kind).Escalate(tree.StateUnknown).Note(err.Error()))
		return
	}

	candidates := r.arrayObjects(ctx, kind)
	cands := handleCandidates(handle)

	var matches []*unstructured.Unstructured
	var matchedBy []string
	for _, obj := range candidates {
		for _, field := range spec.match {
			var val string
			if field == "name" {
				val = obj.GetName()
			} else {
				val = fieldStr(obj.Object, "spec", field)
			}
			if val == "" {
				continue
			}
			if matchesAny(val, cands) {
				matches = append(matches, obj)
				matchedBy = append(matchedBy, field)
				break
			}
		}
	}

	if len(matches) == 0 {
		state := tree.StateMissing
		if lk.soft {
			// Expected once the source volume has been reclaimed, so this must not
			// read as a fault or drive a non-zero exit.
			state = tree.StateInfo
		}
		n := parent.Add(tree.New(lk.prefix + kind + " for handle " + truncate(handle, 48)).SetState(state))
		n.Note(fmt.Sprintf("scanned %d %s objects, matched none on %s",
			len(candidates), kind, strings.Join(spec.match, "/")))
		n.Note("tried: " + strings.Join(cands, " | "))
		if lk.soft {
			n.Note("expected if the source volume has been reclaimed; the handle above is still what to search for on the array")
			return
		}
		r.rep.Problem(tree.StateMissing, kind+" lookup",
			fmt.Sprintf("no %s matches CSI handle %q", kind, handle),
			"either the array-side object was deleted out from under the cluster, or this driver names it differently -- compare with `kubectl get "+strings.ToLower(kind)+"s -o yaml` and pass --array-snapshot-kind if the CR differs")
		return
	}

	if len(matches) > 1 {
		r.rep.Problem(tree.StateWarn, kind+" lookup",
			fmt.Sprintf("%d %s objects match handle %q", len(matches), kind, handle),
			"ambiguous match; all candidates are shown")
	}

	for i, obj := range matches {
		n := parent.Add(tree.New(lk.prefix+kind+"/"+obj.GetName()).
			Object(kind, obj.GetNamespace(), obj.GetName(), string(obj.GetUID())))
		n.Attr("matchedOn", matchedBy[i])
		if lk.checkParent {
			r.checkSnapshotParentVolume(ctx, obj, n)
		}
		for _, f := range spec.show {
			n.Attr(f, fieldStr(obj.Object, "spec", f))
		}
		// storageBackends carries the actual array endpoints and targets, which is
		// what you need to log in to the right FlashArray.
		for _, raw := range nslice(obj.Object, "spec", "storageBackends") {
			m := asMap(raw)
			parts := []string{}
			if v := fieldStr(m, "id"); v != "" {
				parts = append(parts, "id="+v)
			}
			if v := fieldStr(m, "iqn"); v != "" {
				parts = append(parts, "iqn="+v)
			}
			if v := fieldStr(m, "nqn"); v != "" {
				parts = append(parts, "nqn="+v)
			}
			if t := stringSlice(nslice(m, "targets")); len(t) > 0 {
				parts = append(parts, "targets="+strings.Join(t, ","))
			}
			if len(parts) > 0 {
				n.Note("backend " + strings.Join(parts, " "))
			}
		}
	}
}

// arrayObjects lists a vendor kind once per run. These CRs may be cluster-scoped
// or namespaced depending on the driver version, so list without a namespace
// filter and let the match decide.
func (r *Resolver) arrayObjects(ctx context.Context, kind string) []*unstructured.Unstructured {
	if objs, ok := r.arrayCache[kind]; ok {
		return objs
	}
	objs, err := r.src.List(ctx, kind, "", "")
	if err != nil {
		objs = nil
	}
	r.arrayCache[kind] = objs
	return objs
}

func matchesAny(val string, candidates []string) bool {
	for _, c := range candidates {
		if strings.EqualFold(val, c) {
			return true
		}
	}
	return false
}

// fieldStr reads a scalar field of any JSON type as a display string.
func fieldStr(o map[string]interface{}, fields ...string) string {
	v, found, err := unstructured.NestedFieldNoCopy(o, fields...)
	if !found || err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// checkSnapshotParentVolume tests whether the volume a snapshot was taken from
// still exists on the array.
//
// On Pure FlashArray a snapshot is a child of its source volume: destroying the
// volume destroys its snapshots, and after the eradication window they are
// unrecoverable. Kubernetes does not re-verify this -- a VolumeSnapshot keeps
// readyToUse=true forever -- so a snapshot can appear perfectly healthy while
// being impossible to restore. Pure's snapshotName encodes its parent as
// "<volume>.<suffix>", which lets us check.
func (r *Resolver) checkSnapshotParentVolume(ctx context.Context, snap *unstructured.Unstructured, node *tree.Node) {
	snapName := fieldStr(snap.Object, "spec", "snapshotName")
	idx := strings.LastIndex(snapName, ".")
	if idx <= 0 {
		// Not the "<volume>.<suffix>" form. Drivers differ here: one Portworx
		// version names snapshots after their parent volume, another uses a flat
		// "px_<cluster>-snapshot-<uuid>". Saying nothing would read as "checked and
		// fine", so state that this method does not apply and point at the check
		// that does.
		node.Note("cannot infer a parent volume from snapshotName " + truncate(snapName, 60) +
			": this driver does not encode one, so parent-death cannot be judged from the name -- " +
			"the origin volume lookup on the VolumeSnapshotContent above is the check that applies here")
		return
	}
	parent := snapName[:idx]

	vols := r.arrayObjects(ctx, r.opt.ArrayVolumeKind)
	if len(vols) == 0 {
		return // cannot tell; stay silent rather than imply either answer
	}
	for _, v := range vols {
		if fieldStr(v.Object, "spec", "volumeName") == parent || v.GetName() == parent {
			node.Note("parent volume " + parent + " still exists on the array")
			return
		}
	}
	node.Escalate(tree.StateWarn)
	node.Note("parent volume " + parent + " NOT found among " + fmt.Sprint(len(vols)) + " array volumes")
	r.rep.Problem(tree.StateWarn, node.Label,
		fmt.Sprintf("the volume this snapshot was taken from (%s) no longer exists on the array", parent),
		"on FlashArray a snapshot is a child of its source volume: destroying the volume destroys its snapshots, and they are unrecoverable after the eradication window. Kubernetes still reports readyToUse=true because nothing re-verifies it, so this snapshot may be unrestorable -- confirm on the array before relying on it")
}

func shortSource(s source.Source) string {
	if strings.HasPrefix(s.Describe(), "dump") {
		return "dump"
	}
	return "cluster"
}
