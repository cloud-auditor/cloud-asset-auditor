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
}

// Confidence levels.
const (
	ConfidenceExact     = "exact"
	ConfidenceHeuristic = "heuristic"
)

// Canonical edge kinds. Resolvers should use these constants so renderers
// and downstream consumers don't have to enumerate a free-form string set.
const (
	EdgeKindDNS            = "dns"             // DNS record → resolved target
	EdgeKindWAF            = "waf"             // CDN/security rule → protected zone
	EdgeKindLBBackend      = "lb-backend"      // Cloud LB → backend pool member
	EdgeKindGatewayRoute   = "gateway-route"   // Ingress / Gateway rule → matched Service
	EdgeKindServiceBackend = "service-backend" // Service → backing pod set / endpoint
)
