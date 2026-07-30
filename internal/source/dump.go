package source

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Dump reads objects from a directory tree of YAML/JSON on disk -- an
// `oc adm must-gather` output, an `oc get -o yaml` capture, or anything in
// between. This is what lets you resolve a chain whose PVCs were already
// deleted, or debug without cluster access.
type Dump struct {
	root string
	// byKind: lowercase kind -> "namespace/name" -> object
	byKind map[string]map[string]*unstructured.Unstructured
	// groups: lowercase kind -> observed apiVersion groups
	groups map[string]map[string]bool
	// namespaced: lowercase kind -> whether objects carried a namespace
	namespaced map[string]bool
	files      int
	objects    int
}

// NewDump indexes every manifest under root.
func NewDump(root string) (*Dump, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("reading dump %s: %w", root, err)
	}
	d := &Dump{
		root:       root,
		byKind:     map[string]map[string]*unstructured.Unstructured{},
		groups:     map[string]map[string]bool{},
		namespaced: map[string]bool{},
	}
	if !st.IsDir() {
		if err := d.loadFile(root); err != nil {
			return nil, err
		}
		return d, nil
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort the whole index
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			_ = d.loadFile(path) // a single malformed file must not kill the run
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if d.objects == 0 {
		return nil, fmt.Errorf("no Kubernetes objects found under %s", root)
	}
	return d, nil
}

func (d *Dump) Describe() string {
	return fmt.Sprintf("dump %s (%d objects from %d files)", d.root, d.objects, d.files)
}

func (d *Dump) loadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d.files++
	for _, doc := range splitYAMLDocs(raw) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal(doc, &m); err != nil || m == nil {
			continue
		}
		d.absorb(&unstructured.Unstructured{Object: m})
	}
	return nil
}

// absorb stores an object, transparently unpacking List wrappers so that
// `oc get vmsnapshot -o yaml` output (kind: List) indexes as its items.
func (d *Dump) absorb(obj *unstructured.Unstructured) {
	kind := obj.GetKind()
	if kind == "" {
		return
	}
	if strings.HasSuffix(kind, "List") {
		items, found, _ := unstructured.NestedSlice(obj.Object, "items")
		if found {
			for _, it := range items {
				if m, ok := it.(map[string]interface{}); ok {
					child := &unstructured.Unstructured{Object: m}
					// Items inside a List often omit apiVersion/kind; inherit
					// them from the wrapper.
					if child.GetKind() == "" {
						child.SetKind(strings.TrimSuffix(kind, "List"))
						child.SetAPIVersion(obj.GetAPIVersion())
					}
					d.absorb(child)
				}
			}
			return
		}
	}
	if obj.GetName() == "" {
		return
	}
	k := normKind(kind)
	if d.byKind[k] == nil {
		d.byKind[k] = map[string]*unstructured.Unstructured{}
		d.groups[k] = map[string]bool{}
	}
	gv, _ := schema.ParseGroupVersion(obj.GetAPIVersion())
	d.groups[k][gv.Group] = true
	if obj.GetNamespace() != "" {
		d.namespaced[k] = true
	}
	d.byKind[k][obj.GetNamespace()+"/"+obj.GetName()] = obj
	d.objects++
}

func (d *Dump) ResolveKind(kind, group string) (Mapping, error) {
	k := normKind(kind)
	groups, ok := d.groups[k]
	if !ok {
		return Mapping{}, fmt.Errorf("%w: kind %q absent from %s", ErrKindUnknown, kind, d.root)
	}
	cands := make([]Mapping, 0, len(groups))
	for g := range groups {
		cands = append(cands, Mapping{
			Resource:   schema.GroupVersionResource{Group: g, Resource: k + "s"},
			Namespaced: d.namespaced[k],
		})
	}
	return pickMapping(kind, group, cands)
}

func (d *Dump) Get(ctx context.Context, ref Ref) (*unstructured.Unstructured, error) {
	k := normKind(ref.Kind)
	objs, ok := d.byKind[k]
	if !ok {
		return nil, fmt.Errorf("%w: kind %q absent from %s", ErrKindUnknown, ref.Kind, d.root)
	}
	if obj, ok := objs[ref.Namespace+"/"+ref.Name]; ok {
		return obj, nil
	}
	// A dump may record cluster-scoped objects with no namespace even for kinds
	// we consider namespaced, and vice versa; fall back to a name-only match.
	for key, obj := range objs {
		if key[strings.Index(key, "/")+1:] == ref.Name {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

func (d *Dump) List(ctx context.Context, kind, group, namespace string) ([]*unstructured.Unstructured, error) {
	k := normKind(kind)
	objs, ok := d.byKind[k]
	if !ok {
		return nil, fmt.Errorf("%w: kind %q absent from %s", ErrKindUnknown, kind, d.root)
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		if namespace != "" && obj.GetNamespace() != "" && obj.GetNamespace() != namespace {
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// splitYAMLDocs splits on lines that are exactly a document separator.
func splitYAMLDocs(raw []byte) [][]byte {
	lines := bytes.Split(raw, []byte("\n"))
	var docs [][]byte
	var cur [][]byte
	for _, ln := range lines {
		if bytes.Equal(bytes.TrimRight(ln, " \r\t"), []byte("---")) {
			docs = append(docs, bytes.Join(cur, []byte("\n")))
			cur = nil
			continue
		}
		cur = append(cur, ln)
	}
	docs = append(docs, bytes.Join(cur, []byte("\n")))
	return docs
}
