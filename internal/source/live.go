package source

import (
	"context"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// Live reads from the cluster in the current kubecontext using the dynamic
// client, so no CRD Go types need to be vendored.
type Live struct {
	dyn       dynamic.Interface
	disco     discovery.DiscoveryInterface
	host      string
	defaultNS string

	once     sync.Once
	kindIdx  map[string][]Mapping
	discoErr error
}

// NewLive builds a Live source from standard kubectl config flags.
func NewLive(flags *genericclioptions.ConfigFlags) (*Live, error) {
	cfg, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	// The traversal issues many small serial reads; the default client-side
	// throttle of 5 QPS makes a wide VM feel broken.
	cfg.QPS = 50
	cfg.Burst = 100

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	ns, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		ns = "default"
	}
	return &Live{dyn: dyn, disco: disco, host: cfg.Host, defaultNS: ns}, nil
}

// DefaultNamespace is the namespace from the kubecontext, used when -n is absent.
func (l *Live) DefaultNamespace() string { return l.defaultNS }

func (l *Live) Describe() string { return "cluster " + l.host }

// index builds Kind -> mappings once, from server-preferred resources. Partial
// discovery failures are tolerated: an unreachable aggregated API server must not
// stop us from resolving the kinds that did come back.
func (l *Live) index() (map[string][]Mapping, error) {
	l.once.Do(func() {
		lists, err := l.disco.ServerPreferredResources()
		if len(lists) == 0 && err != nil {
			l.discoErr = fmt.Errorf("API discovery failed: %w", err)
			return
		}
		idx := map[string][]Mapping{}
		for _, list := range lists {
			if list == nil {
				continue
			}
			gv, err := schema.ParseGroupVersion(list.GroupVersion)
			if err != nil {
				continue
			}
			for _, r := range list.APIResources {
				// Skip subresources such as pods/log.
				if len(r.Name) == 0 || containsSlash(r.Name) {
					continue
				}
				k := normKind(r.Kind)
				idx[k] = append(idx[k], Mapping{
					Resource:   gv.WithResource(r.Name),
					Namespaced: r.Namespaced,
				})
			}
		}
		l.kindIdx = idx
	})
	return l.kindIdx, l.discoErr
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

func (l *Live) ResolveKind(kind, group string) (Mapping, error) {
	idx, err := l.index()
	if err != nil {
		return Mapping{}, err
	}
	return pickMapping(kind, group, idx[normKind(kind)])
}

func (l *Live) Get(ctx context.Context, ref Ref) (*unstructured.Unstructured, error) {
	m, err := l.ResolveKind(ref.Kind, ref.Group)
	if err != nil {
		return nil, err
	}
	if ref.Name == "" {
		return nil, fmt.Errorf("%w: empty name for kind %s", ErrNotFound, ref.Kind)
	}
	var ri dynamic.ResourceInterface = l.dyn.Resource(m.Resource)
	if m.Namespaced {
		ns := ref.Namespace
		if ns == "" {
			ns = l.defaultNS
		}
		ri = l.dyn.Resource(m.Resource).Namespace(ns)
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, translate(err, ref)
	}
	return obj, nil
}

func (l *Live) List(ctx context.Context, kind, group, namespace string) ([]*unstructured.Unstructured, error) {
	m, err := l.ResolveKind(kind, group)
	if err != nil {
		return nil, err
	}
	var ri dynamic.ResourceInterface = l.dyn.Resource(m.Resource)
	if m.Namespaced && namespace != "" {
		ri = l.dyn.Resource(m.Resource).Namespace(namespace)
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, translate(err, Ref{Kind: kind, Namespace: namespace})
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// translate maps API errors onto our sentinels so the renderer can tell a
// dangling reference apart from an RBAC gap.
func translate(err error, ref Ref) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w: %s", ErrForbidden, ref)
	case apierrors.IsMethodNotSupported(err):
		return fmt.Errorf("%w: %s", ErrKindUnknown, ref)
	default:
		return fmt.Errorf("reading %s: %w", ref, err)
	}
}
