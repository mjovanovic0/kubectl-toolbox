// Package source abstracts "where do objects come from" so the resolver can walk
// the same reference graph against a live cluster or an offline must-gather dump.
package source

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// ErrNotFound means the lookup was well-formed but the object does not exist.
	// This is the interesting case for debugging: a dangling reference.
	ErrNotFound = errors.New("not found")

	// ErrKindUnknown means the resource type itself is not registered (CRD not
	// installed, or absent from the dump).
	ErrKindUnknown = errors.New("resource type not registered")

	// ErrForbidden means we are not allowed to read it, which must never be
	// reported as a missing object.
	ErrForbidden = errors.New("forbidden")
)

// Ref identifies one object to fetch. Group may be empty, in which case the
// Source resolves it from Kind alone -- that is what lets us reach vendor CRs
// such as PureSnapshot whose API group is not documented.
type Ref struct {
	Kind      string
	Group     string
	Namespace string
	Name      string
}

func (r Ref) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s/%s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s/%s (ns %s)", r.Kind, r.Name, r.Namespace)
}

// Mapping is the resolved location of a Kind.
type Mapping struct {
	Resource   schema.GroupVersionResource
	Namespaced bool
}

// Source reads Kubernetes objects by kind and name.
type Source interface {
	// Get fetches a single object. Returns ErrNotFound, ErrKindUnknown or
	// ErrForbidden (wrapped) so callers can distinguish a dangling reference
	// from an uninstalled CRD from an RBAC gap.
	Get(ctx context.Context, ref Ref) (*unstructured.Unstructured, error)

	// List returns every object of a kind, optionally restricted to a namespace.
	List(ctx context.Context, kind, group, namespace string) ([]*unstructured.Unstructured, error)

	// ResolveKind reports where a Kind lives.
	ResolveKind(kind, group string) (Mapping, error)

	// Describe names the backing store, for the report header.
	Describe() string
}

// preferredGroups pins the API group for kinds whose name is ambiguous across
// groups, so a stray CRD cannot shadow the real one. Kinds absent from this map
// (PureSnapshot, PureVolume, anything vendor-specific) are resolved by scanning
// discovery for a matching Kind.
var preferredGroups = map[string]string{
	"VirtualMachine":                "kubevirt.io",
	"VirtualMachineInstance":        "kubevirt.io",
	"VirtualMachineSnapshot":        "snapshot.kubevirt.io",
	"VirtualMachineSnapshotContent": "snapshot.kubevirt.io",
	"VirtualMachineRestore":         "snapshot.kubevirt.io",
	"VirtualMachineClone":           "clone.kubevirt.io",
	"DataVolume":                    "cdi.kubevirt.io",
	"DataSource":                    "cdi.kubevirt.io",
	"DataImportCron":                "cdi.kubevirt.io",
	"VolumeSnapshot":                "snapshot.storage.k8s.io",
	"VolumeSnapshotContent":         "snapshot.storage.k8s.io",
	"VolumeSnapshotClass":           "snapshot.storage.k8s.io",
	"PersistentVolumeClaim":         "",
	"PersistentVolume":              "",
	"StorageClass":                  "storage.k8s.io",
	"VolumeAttachment":              "storage.k8s.io",
	// Events must come from core/v1: events.k8s.io renames involvedObject and is
	// not what `kubectl get events` shows.
	"Event": "",
	"Pod":   "",
	"Node":  "",
}

// PreferredGroup returns the pinned group for a kind and whether it was pinned.
func PreferredGroup(kind string) (string, bool) {
	g, ok := preferredGroups[kind]
	return g, ok
}

// pickMapping chooses among several candidate mappings for one Kind.
func pickMapping(kind, groupHint string, candidates []Mapping) (Mapping, error) {
	if len(candidates) == 0 {
		return Mapping{}, fmt.Errorf("%w: kind %q", ErrKindUnknown, kind)
	}
	if groupHint != "" {
		for _, c := range candidates {
			if c.Resource.Group == groupHint {
				return c, nil
			}
		}
		return Mapping{}, fmt.Errorf("%w: kind %q in group %q", ErrKindUnknown, kind, groupHint)
	}
	if want, ok := preferredGroups[kind]; ok {
		for _, c := range candidates {
			if c.Resource.Group == want {
				return c, nil
			}
		}
	}
	return candidates[0], nil
}

func normKind(kind string) string { return strings.ToLower(kind) }
