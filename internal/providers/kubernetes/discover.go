package kubernetes

import (
	"errors"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// resourceTarget is one (GVR, namespaced) pair that survived discovery
// filtering and is ready to feed into the dynamic lister.
type resourceTarget struct {
	GVR        schema.GroupVersionResource
	Namespaced bool
	Kind       string // for nicer error wrapping
}

// discoverResources walks every (group, version, resource) the cluster
// reports as preferred and applies the filtering rules.
//
// ServerPreferredResources commonly returns a partial result *and* an
// error — that's how aggregated APIs whose backing service is down show
// up. We treat that case as a warning (returned alongside the partial
// slice) so the audit continues.
func (p *Provider) discoverResources() ([]resourceTarget, error) {
	if err := p.ensureClients(); err != nil {
		return nil, err
	}

	lists, discErr := p.discovery.ServerPreferredResources()
	var partial *discovery.ErrGroupDiscoveryFailed
	if discErr != nil && !errors.As(discErr, &partial) {
		return nil, fmt.Errorf("discover preferred resources: %w", discErr)
	}
	return filterResources(lists, p.cfg.ExcludeEvents), discErr // discErr may be nil or partial
}

// filterResources is the pure, table-test-friendly half of discovery: it
// turns the API-resource lists the server reports into the slice of
// listable, non-subresource (GVR, namespaced) targets we feed to the
// dynamic client. Output is sorted by GVR string so two runs against the
// same cluster produce the same ordering.
//
// excludeEvents (from --kube-exclude-events) drops the Event GVRs here, before
// any List call — Event objects are high-volume and ephemeral, so skipping the
// whole resource is far cheaper than listing then discarding per item.
func filterResources(lists []*metav1.APIResourceList, excludeEvents bool) []resourceTarget {
	var out []resourceTarget
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if isSubresource(r.Name) || !supportsList(r.Verbs) {
				continue
			}
			if isEventResource(gv.Group, r.Name) {
				// Events are one stored object projected through two API
				// groups: core "" and events.k8s.io. Discovery returns both,
				// so listing both collects every event twice — same UID, same
				// name, two assets. That double-counts the inventory and, since
				// asset identity is (provider, id), puts two entries on one key
				// in diff, the topology index, and the cache.
				//
				// Keep the core group: it is the one every cluster serves
				// (events.k8s.io does not exist before 1.19) and the one whose
				// payload shape — involvedObject — the rest of the ecosystem
				// reads. --kube-exclude-events still drops both.
				if excludeEvents || gv.Group != "" {
					continue
				}
			}
			out = append(out, resourceTarget{
				GVR: schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: r.Name,
				},
				Namespaced: r.Namespaced,
				Kind:       r.Kind,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GVR.String() < out[j].GVR.String()
	})
	return out
}
