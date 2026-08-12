package core

// AssetRef is the minimal identifying subset of an Asset — used in Edge.From
// and Edge.To so a topology graph doesn't have to duplicate full Asset
// records (or pin them to specific in-memory pointers, which would break
// JSON round-tripping).
type AssetRef struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id,omitempty"`
	Type      string `json:"type"`
	ID        string `json:"id"`
}

// AsRef returns this Asset's identifying tuple. Provider+ID is unique in
// practice, but Type + AccountID help disambiguate when collisions happen
// across providers and make rendered graphs readable.
func (a Asset) AsRef() AssetRef {
	return AssetRef{
		Provider:  a.Provider,
		AccountID: a.AccountID,
		Type:      a.Type,
		ID:        a.ID,
	}
}

// Edge is a derived relationship between two Assets — produced by the
// topology package, never by a provider directly. Confidence makes it
// explicit when an edge is a heuristic guess (matched by IP/hostname
// across cloud boundaries) versus a strong match (e.g. an OCID embedded
// in a Service annotation).
type Edge struct {
	From       AssetRef `json:"from"`
	To         AssetRef `json:"to"`
	Kind       string   `json:"kind"`
	Hostname   string   `json:"hostname,omitempty"`
	Port       int      `json:"port,omitempty"`
	Confidence string   `json:"confidence"`

	// Count is how many underlying edges this one stands for. It is 0 on a
	// resolver-produced edge (which always represents exactly itself) and set
	// only by the topology collapse transform, where one rendered arrow
	// summarises many. omitempty keeps the JSON contract unchanged for the
	// uncollapsed graph that every existing consumer sees.
	Count int `json:"count,omitempty"`
}

// Confidence levels.
const (
	ConfidenceExact     = "exact"
	ConfidenceHeuristic = "heuristic"
)

// Canonical edge kinds. Resolvers should use these constants so renderers
// and downstream consumers don't have to enumerate a free-form string set.
const (
	EdgeKindDNS                = "dns"                 // DNS record → resolved target
	EdgeKindWAF                = "waf"                 // CDN/security rule → protected zone
	EdgeKindLBBackend          = "lb-backend"          // Cloud LB → backend pool member
	EdgeKindGatewayRoute       = "gateway-route"       // Ingress / Gateway rule → matched Service
	EdgeKindServiceBackend     = "service-backend"     // Service → backing pod set / endpoint
	EdgeKindNetworkContainment = "network-containment" // resource → its containing network (subnet / VCN)

	// Traffic-flow edges answer "who may talk to whom", derived from policy
	// documents (Kubernetes NetworkPolicy, Tailscale ACLs, NetBird policies)
	// rather than from a request path. They are directional source → target
	// and split by verdict so a renderer can style a denial differently from
	// a grant — collapsing both into one kind would draw a firewall's blocks
	// as if they were reachability.
	EdgeKindTrafficAllow = "traffic-allow" // policy grants source → target
	EdgeKindTrafficDeny  = "traffic-deny"  // policy denies source → target
)

// TrafficEdgeKind maps a policy verdict onto the matching edge kind. Policy
// engines spell the allow/deny verdict differently (accept/allow vs
// drop/deny/reject); this is the single place that normalisation lives.
func TrafficEdgeKind(action string) string {
	switch action {
	case "drop", "deny", "reject":
		return EdgeKindTrafficDeny
	default:
		return EdgeKindTrafficAllow
	}
}
