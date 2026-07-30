package resolve

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// maxEventsPerObject bounds how many events one node reports. The newest few
// carry the diagnosis; older repeats are noise.
const maxEventsPerObject = 4

// event is one flattened Kubernetes event.
type event struct {
	kind      string
	name      string
	namespace string
	uid       string
	typ       string
	reason    string
	message   string
	count     int64
	last      string
}

// decorateWithEvents attaches Kubernetes events to nodes after the tree is built.
//
// Events are the first thing you check by hand when a PVC will not bind or a VM
// will not start, and they are the one signal that explains *why* rather than
// *what*. By default they are attached only to nodes already flagged as
// unhealthy, so a clean report stays readable; AllEvents attaches them
// everywhere.
func (r *Resolver) decorateWithEvents(ctx context.Context) {
	if r.opt.NoEvents || r.rep == nil || r.rep.Root == nil {
		return
	}

	// Collect the identities we actually care about first, so a namespace with
	// thousands of events is listed at most once.
	var targets []*tree.Node
	r.rep.Root.Walk(func(n *tree.Node) {
		if n.Ref == nil {
			return
		}
		if r.opt.AllEvents || n.Unhealthy() {
			targets = append(targets, n)
		}
	})
	if len(targets) == 0 {
		return
	}

	idx := r.eventIndex(ctx)
	if idx == nil {
		return
	}
	for _, n := range targets {
		evs := idx[eventKey(n.Ref.Kind, n.Ref.Name)]
		if len(evs) == 0 {
			continue
		}
		// Prefer Warnings, then most recent.
		sort.SliceStable(evs, func(i, j int) bool {
			if (evs[i].typ == "Warning") != (evs[j].typ == "Warning") {
				return evs[i].typ == "Warning"
			}
			return evs[i].last > evs[j].last
		})
		shown := evs
		if len(shown) > maxEventsPerObject {
			shown = shown[:maxEventsPerObject]
		}
		for _, e := range shown {
			n.Note(e.describe())
		}
		if len(evs) > len(shown) {
			n.Note(fmt.Sprintf("event ... and %d older event(s) not shown", len(evs)-len(shown)))
		}
		r.promoteFailureEvent(n, evs)
	}
}

// promoteFailureEvent lifts the newest Warning into the problem list.
//
// A note buried in the tree is not where anyone looks first, and the event is
// frequently the actual diagnosis -- the CSI driver's own words about why it
// cannot provision, attach or mount. Reporting it as a problem puts the cause
// next to the symptom.
func (r *Resolver) promoteFailureEvent(n *tree.Node, evs []event) {
	for _, e := range evs {
		if e.typ != "Warning" {
			continue
		}
		where := n.Ref.Kind + "/" + n.Ref.Name
		msg := fmt.Sprintf("%s reports %s", where, e.reason)
		if e.count > 1 {
			msg += fmt.Sprintf(" (%d times, latest %s)", e.count, e.last)
		}
		msg += ": " + truncate(strings.TrimSpace(e.message), 300)
		r.rep.Problem(tree.StateWarn, where, msg, failureEventHint(e))
		return // newest Warning only; the rest are repeats or older attempts
	}
}

// failureEventHint maps well-known event reasons to where to look next.
func failureEventHint(e event) string {
	switch e.reason {
	case "ProvisioningFailed":
		// The driver's message is authoritative. Errors naming a missing volume or
		// snapshot mean the array no longer has the object Kubernetes still records.
		if mentionsMissingArrayObject(e.message) {
			return "the storage array reports the source object does not exist, even though Kubernetes still records it as ready -- treat the snapshot as gone and verify on the array before trusting any readyToUse flag"
		}
		return "provisioning is failing in the CSI driver, not in Kubernetes; the message above is the driver's own reason"
	case "FailedAttachVolume", "FailedMount", "FailedMapVolume":
		return "the volume exists but cannot be presented to the node; check the CSI node plugin on that node and the array-side host connection"
	case "VolumeFailedDelete":
		return "the PV cannot be reclaimed; the array-side volume will leak until this is resolved"
	case "FailedScheduling":
		return "no node can run the pod; with WaitForFirstConsumer storage this also blocks the PVC from binding"
	case "SnapshotCreationFailed", "SnapshotContentCreationFailed":
		return "the CSI driver could not create the array-side snapshot; the VolumeSnapshot will never become ready"
	}
	return "this is the controller's own account of the failure"
}

// mentionsMissingArrayObject spots driver errors that mean the backing object is
// absent on the array rather than merely unavailable.
func mentionsMissingArrayObject(msg string) bool {
	m := strings.ToLower(msg)
	for _, phrase := range []string{
		"no such volume or snapshot",
		"no such volume",
		"no such snapshot",
		"does not exist",
		"not found",
	} {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

func (e event) describe() string {
	var sb strings.Builder
	sb.WriteString("event ")
	sb.WriteString(e.typ)
	sb.WriteString(" ")
	sb.WriteString(e.reason)
	if e.count > 1 {
		fmt.Fprintf(&sb, " x%d", e.count)
	}
	if e.last != "" {
		sb.WriteString(" @" + e.last)
	}
	sb.WriteString(": ")
	sb.WriteString(truncate(strings.TrimSpace(e.message), 220))
	return sb.String()
}

func eventKey(kind, name string) string { return kind + "/" + name }

// eventIndex lists events once per run and groups them by involved object.
func (r *Resolver) eventIndex(ctx context.Context) map[string][]event {
	if r.events != nil {
		return r.events
	}
	r.events = map[string][]event{}

	// core/v1 Events carry involvedObject; events.k8s.io/v1 renames it to
	// "regarding". Read both shapes so either API version works, and so a
	// must-gather dump containing either form is usable.
	for _, kind := range []string{"Event"} {
		objs, err := r.src.List(ctx, kind, "", "")
		if err != nil {
			continue
		}
		for _, o := range objs {
			e := flattenEvent(o)
			if e.kind == "" || e.name == "" {
				continue
			}
			key := eventKey(e.kind, e.name)
			r.events[key] = append(r.events[key], e)
		}
	}
	return r.events
}

func flattenEvent(o *unstructured.Unstructured) event {
	involved := nmap(o.Object, "involvedObject")
	if len(involved) == 0 {
		involved = nmap(o.Object, "regarding")
	}
	e := event{
		kind:      nstr(involved, "kind"),
		name:      nstr(involved, "name"),
		namespace: nstr(involved, "namespace"),
		uid:       nstr(involved, "uid"),
		typ:       nstr(o.Object, "type"),
		reason:    nstr(o.Object, "reason"),
		message:   nstr(o.Object, "message"),
	}
	if e.typ == "" {
		e.typ = "Normal"
	}
	if c, found := nint(o.Object, "count"); found {
		e.count = c
	}
	// core/v1 uses lastTimestamp; events.k8s.io uses deprecatedLastTimestamp or
	// series.lastObservedTime, with eventTime as a fallback.
	for _, f := range [][]string{
		{"lastTimestamp"},
		{"deprecatedLastTimestamp"},
		{"series", "lastObservedTime"},
		{"eventTime"},
		{"firstTimestamp"},
	} {
		if v := nstr(o.Object, f...); v != "" {
			e.last = v
			break
		}
	}
	return e
}
