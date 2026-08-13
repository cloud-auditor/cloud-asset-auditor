package insight

// Network insights: what the estate's address surface looks like, where it is
// entered, and where its own DNS points at nothing.
//
// Everything here comes from two sources — the addresses providers publish as
// tags or in a Raw payload, and the graph internal/topology infers from them —
// and that bound is worth stating once, because every Caveat below is a
// specialisation of it. An inventory sees an address only when the provider
// that owns it chose to report one. OCI compute instances report no VNIC
// address at all (the collector does not read VNIC attachments), Kubernetes pod
// and node addresses arrive only with --include-raw, and nothing reports an
// address that is merely reserved. So every count in this file is a floor, and
// an address block that looks empty is evidence of nothing.
//
// Addresses are read with topology.AssetAddresses — the same parser the graph
// joins on, so a report about the estate's surface and a diagram of it cannot
// disagree about what the estate publishes. The three supplements this file
// adds (supplementalAddresses) are exactly the addresses the graph declines to
// index, because indexing them there would mint edges.

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

func init() {
	Register(publicAddresses{})
	Register(privateAddresses{})
	Register(networkGateways{})
	Register(ingressPoints{})
	Register(danglingDNS{})
	Register(overlappingCIDRs{})
}

// ----------------------------------------------------------------------
// addresses: classification
// ----------------------------------------------------------------------

// cgnat is RFC 6598 shared address space. It gets a named variable rather than
// being folded into an "is it private" helper because of what lives there in
// this project: both mesh VPN providers (NetBird, Tailscale) allocate their
// overlay addresses out of 100.64/10, so an estate running either has hundreds
// of 100.x addresses. Counting those as public would report every laptop in
// the mesh as an internet-facing endpoint; counting them as RFC1918 would file
// them under a VCN's address plan, which is not where they are either.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicAddr reports whether the internet can route to an address.
//
// IsGlobalUnicast already excludes loopback, link-local, multicast, the
// unspecified address and the IPv4 broadcast; IsPrivate covers RFC1918 and
// IPv6 unique-local. CGNAT is the one range Go has no predicate for and the one
// this estate is most likely to be full of.
//
// Documentation ranges (192.0.2.0/24, 2001:db8::/32 and friends) are NOT
// excluded. They are globally scoped unicast, and an estate that has aimed a
// record at one has aimed it outside itself, which is the question being asked.
func publicAddr(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

// privateAddr reports whether an address belongs to somebody's private address
// plan — RFC1918, IPv6 unique-local, or the mesh overlay range.
//
// Loopback and link-local are deliberately not private *addresses* for this
// purpose: 127.0.0.1 and 169.254.x are per-host facts that appear identically
// in every estate, and listing them beside a VCN's subnets would say nothing
// about the estate's address plan.
func privateAddr(a netip.Addr) bool { return a.IsPrivate() || cgnat.Contains(a) }

// parseAddr parses one address literal, unmapping the IPv4-in-IPv6 form so
// "::ffff:10.0.0.1" and "10.0.0.1" are one address rather than two.
func parseAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// ----------------------------------------------------------------------
// addresses: extraction
// ----------------------------------------------------------------------

// heldAddr is one address an asset answers on. "Held" is the operative word: a
// DNS record's content is not in here, because a record is a claim about
// somebody else's address (see topology.Addresses for the same split).
type heldAddr struct {
	addr  netip.Addr
	asset core.Asset
}

// heldAddresses walks the inventory once and returns every address something
// in it answers on, in the canonical asset order so the result is
// deterministic.
//
// It is recomputed per insight rather than cached on the Input: the walk is one
// pass over an already-built slice and the only expensive part, parsing Raw, is
// memoized by the Input itself, so caching here would buy a few microseconds at
// the cost of a mutable field on a struct documented as read-only.
func heldAddresses(in *Input) []heldAddr {
	var out []heldAddr
	for _, a := range in.Assets {
		for _, s := range topology.AssetAddresses(a).IPs {
			if addr, ok := parseAddr(s); ok {
				out = append(out, heldAddr{addr: addr, asset: a})
			}
		}
		for _, addr := range supplementalAddresses(in, a) {
			out = append(out, heldAddr{addr: addr, asset: a})
		}
	}
	return out
}

// supplementalAddresses reads the addresses topology.AssetAddresses leaves
// alone. They are read here rather than added there because that parser feeds
// the index every resolver joins on: an address added to it becomes an edge,
// and these three would each invent one that nobody asked for — a DNS record
// aimed at a NAT gateway's egress IP would be drawn as a request path into the
// gateway, and 30,000 pod addresses would put 30,000 new keys in the join
// table to no end.
//
// They matter to an address report for the opposite reason: a NAT gateway's IP
// is one of the few public addresses an OCI estate has, and pod and node
// addresses are most of what actually occupies a private range.
func supplementalAddresses(in *Input, a core.Asset) []netip.Addr {
	var out []netip.Addr
	add := func(s string) {
		if addr, ok := parseAddr(s); ok {
			out = append(out, addr)
		}
	}

	switch a.Type {
	case "oci.nat_gateway":
		// The public address the VCN's outbound traffic is seen as. See
		// providers/oci/network.go — it is a tag, so no Raw is needed.
		add(a.Tags["nat_ip"])

	case "v1.Pod":
		if ip, ok := in.RawString(a, "status.podIP"); ok {
			add(ip)
		}

	case "v1.Node":
		// status.addresses is a list of {type,address}: InternalIP is the
		// address the node holds in the cloud subnet, ExternalIP the public one
		// if it has any, and Hostname a name rather than an address (parseAddr
		// drops it without a type check).
		list, ok := in.RawPath(a, "status.addresses")
		if !ok {
			break
		}
		entries, ok := list.([]any)
		if !ok {
			break
		}
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := entry["address"].(string); ok {
				add(s)
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------
// declared address ranges
// ----------------------------------------------------------------------

// declaredRange is one CIDR block a provider says belongs to this estate.
type declaredRange struct {
	prefix netip.Prefix
	asset  core.Asset
	// network keys the block to the network it belongs to, so two subnets of
	// one VCN are not reported as overlapping each other.
	network string
}

// rangeTags are the tag keys providers publish a CIDR under: oci.subnet's
// cidr_block, oci.vcn's comma-joined cidr_blocks, GCP's ip_cidr_range on a
// Subnetwork, netbird.route's network.
//
// A table rather than "any tag value that parses as a prefix", which is what
// the GCP provider does for addresses. The difference is what the values are
// used for: these feed a containment test whose output is close to an
// accusation ("your DNS points into your own space at something that is not
// there"), and a stray prefix-shaped tag on an unrelated resource would put
// records inside a range nobody owns.
var rangeTags = []string{"cidr_block", "cidr_blocks", "ip_cidr_range", "network"}

// minRangeBits is the shortest prefix accepted as a declared range, per family.
//
// A netbird.route may legitimately carry 0.0.0.0/0 — "route everything through
// this peer" — which is a statement about routing, not about ownership.
// Accepting it would place every address on the internet inside a range this
// estate owns, and the dangling-DNS finding would then flag every record in the
// audit. /8 is the largest block the providers here hand out (10.0.0.0/8 is a
// legal VCN CIDR); /16 is the equivalent floor for IPv6.
var minRangeBits = map[bool]int{true: 8, false: 16} // keyed by prefix.Addr().Is4()

// declaredRanges returns every address block the inventory declares, in
// canonical asset order.
func declaredRanges(in *Input) []declaredRange {
	var out []declaredRange
	for _, a := range in.Assets {
		key := networkKey(a)
		for _, tag := range rangeTags {
			for _, s := range strings.Split(a.Tags[tag], ",") {
				p, err := netip.ParsePrefix(strings.TrimSpace(s))
				if err != nil {
					continue
				}
				p = p.Masked()
				if p.Bits() < minRangeBits[p.Addr().Is4()] {
					continue
				}
				out = append(out, declaredRange{prefix: p, asset: a, network: key})
			}
		}
	}
	return out
}

// networkKey identifies the network a block belongs to. A vcn_id tag is the
// direct answer where a provider publishes one; otherwise the asset stands for
// its own network, which is what a VCN does for the blocks it declares itself.
//
// The bare id is deliberate rather than a provider-qualified one: it has to
// compare equal to the vcn_id tag a subnet carries, or a VCN would end up in a
// different "network" from its own subnets and be reported as overlapping
// them — which is true of every estate and therefore worthless. The ids
// involved are OCIDs and GCP resource names, which do not collide across
// providers; that is the same trade Input.AssetByID documents.
//
// Standing alone can only over-report an overlap — two blocks of one network
// that the tags did not connect. For the provider this actually applies to
// (GCP subnetworks, whose CAI record does not carry the VPC in a tag), that
// over-report cannot happen: GCP refuses to create overlapping subnets inside
// one VPC, so two overlapping ones are in two VPCs by construction.
func networkKey(a core.Asset) string {
	if vcn := a.Tags["vcn_id"]; vcn != "" {
		return vcn
	}
	return a.ID
}

// networkName is what to call a block's network in a row. It resolves the
// vcn_id tag back to the VCN's own name, because "nw-prod-vcn" is what an
// operator recognises and "ocid1.vcn.oc1..aaaa…" is what they have to go and
// look up.
func networkName(in *Input, r declaredRange) string {
	if vcn := r.asset.Tags["vcn_id"]; vcn != "" {
		if a, ok := in.AssetByID(vcn); ok {
			return DisplayName(a)
		}
	}
	return DisplayName(r.asset)
}

// containingRange returns the most specific declared block an address falls
// inside. Most specific rather than first so a row names the subnet rather than
// the VCN that contains it.
func containingRange(ranges []declaredRange, addr netip.Addr) (declaredRange, bool) {
	var best declaredRange
	found := false
	for _, r := range ranges {
		if !r.prefix.Contains(addr) {
			continue
		}
		if !found || r.prefix.Bits() > best.prefix.Bits() {
			best, found = r, true
		}
	}
	return best, found
}

// ----------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------

// shortType is the type as a human says it: the Kubernetes kind out of
// "networking.k8s.io/v1.Ingress", and the provider type verbatim for everything
// else (a reader wants to see "oci.nat_gateway", not "nat_gateway").
func shortType(a core.Asset) string {
	if a.Provider == "kubernetes" {
		if i := strings.LastIndex(a.Type, "."); i >= 0 {
			return a.Type[i+1:]
		}
	}
	return a.Type
}

// downstreamOf returns the assets one hop along the request-path edges leaving
// an asset — what sits *behind* it.
//
// Two edge kinds are excluded, for different reasons. A traffic-allow edge
// says a policy permits a flow, not that this asset forwards to that one:
// following it would put every pod a NetworkPolicy mentions behind a load
// balancer's public address. A network-containment edge points the other way
// round — a gateway points at the VCN that *contains* it — so following it
// forwards reports a resource's container as something behind it.
func downstreamOf(in *Input, a core.Asset) []core.Asset {
	var out []core.Asset
	seen := map[string]bool{}
	for _, e := range in.EdgesFrom(a.AsRef()) {
		switch e.Kind {
		case core.EdgeKindTrafficAllow, core.EdgeKindTrafficDeny, core.EdgeKindNetworkContainment:
			continue
		}
		key := e.To.Provider + "\x00" + e.To.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		if target, ok := in.Asset(e.To); ok {
			out = append(out, target)
		}
	}
	return out
}

// namesPointingAt returns the DNS names the graph says resolve to an asset,
// sorted and deduplicated.
func namesPointingAt(in *Input, a core.Asset) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range in.EdgesTo(a.AsRef()) {
		if e.Kind != core.EdgeKindDNS {
			continue
		}
		name := e.Hostname
		if name == "" {
			if rec, ok := in.Asset(e.From); ok {
				name = DisplayName(rec)
			}
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// refsOf projects assets onto the reference form a Row carries.
func refsOf(assets []core.Asset) []core.AssetRef {
	out := make([]core.AssetRef, 0, len(assets))
	for _, a := range assets {
		out = append(out, a.AsRef())
	}
	return out
}

// namesOf lists up to n display names, with a "+k more" tail. Rows are
// truncated to about 44 columns by the table renderer, so a fact that leads
// with two names and a count survives the cut where a list of nine does not.
func namesOf(assets []core.Asset, n int) string {
	if len(assets) == 0 {
		return ""
	}
	names := make([]string, 0, len(assets))
	for _, a := range assets {
		names = append(names, DisplayName(a))
	}
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:n], ", "), len(names)-n)
}

// pluralize picks between two spellings. render.go's plural() answers the
// simpler question (a bare "s"), which does not help with "address is" vs
// "addresses are".
func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ----------------------------------------------------------------------
// network.public-addresses
// ----------------------------------------------------------------------

type publicAddresses struct{}

func (publicAddresses) ID() string     { return "network.public-addresses" }
func (publicAddresses) Title() string  { return "Internet-routable addresses" }
func (publicAddresses) Family() Family { return FamilyNetwork }

// Run enumerates the addresses the estate answers on that the internet can
// route to, and what the graph puts behind each.
func (x publicAddresses) Run(_ context.Context, in *Input) []Finding {
	holders := map[netip.Addr][]core.Asset{}
	var order []netip.Addr
	seen := map[string]bool{}
	for _, h := range heldAddresses(in) {
		if !publicAddr(h.addr) {
			continue
		}
		key := h.addr.String() + "\x00" + h.asset.Provider + "\x00" + h.asset.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := holders[h.addr]; !ok {
			order = append(order, h.addr)
		}
		holders[h.addr] = append(holders[h.addr], h.asset)
	}
	if len(holders) == 0 {
		return nil
	}

	type entry struct {
		addr    netip.Addr
		assets  []core.Asset
		behind  []core.Asset
		names   []string
		related []core.AssetRef
	}
	entries := make([]entry, 0, len(order))
	assetCount := 0
	for _, addr := range order {
		e := entry{addr: addr, assets: holders[addr]}
		assetCount += len(e.assets)
		for _, a := range e.assets {
			e.behind = append(e.behind, downstreamOf(in, a)...)
			e.names = append(e.names, namesPointingAt(in, a)...)
		}
		e.related = refsOf(e.behind)
		entries = append(entries, e)
	}

	// Ordered by how much sits behind an address rather than numerically: the
	// table prints the first dozen rows, and an address fronting a service is
	// the one worth those twelve lines. The address is the final tiebreak so
	// the order is total and two runs agree.
	sort.SliceStable(entries, func(i, j int) bool {
		if a, b := len(entries[i].behind), len(entries[j].behind); a != b {
			return a > b
		}
		if a, b := len(entries[i].names), len(entries[j].names); a != b {
			return a > b
		}
		return entries[i].addr.Compare(entries[j].addr) < 0
	})

	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		fact := namesOf(e.assets, 2)
		switch {
		case len(e.behind) > 0:
			fact += " → " + namesOf(e.behind, 2)
		case len(e.names) > 0:
			fact += fmt.Sprintf(" (%s)", strings.Join(e.names[:min(len(e.names), 2)], ", "))
		}
		rows = append(rows, Row{
			Label:   e.addr.String(),
			Fact:    fact,
			Related: e.related,
		})
	}

	return []Finding{{
		ID:       "network.public-addresses",
		Title:    "Internet-routable addresses",
		Severity: SeverityNotable,
		Count:    len(entries),
		Summary: fmt.Sprintf("%d internet-routable %s published by %d %s in this inventory.",
			len(entries), pluralize(len(entries), "address is", "addresses are"),
			assetCount, pluralize(assetCount, "asset", "assets")),
		Basis: "every IP literal an asset answers on — the ip_addresses tags OCI and GCP " +
			"publish, a NAT gateway's nat_ip, mesh peer and device addresses, and Kubernetes " +
			"spec.externalIPs / status.loadBalancer from Raw — classified with net/netip. " +
			"RFC1918, IPv6 unique-local, CGNAT 100.64/10 (where the mesh VPNs live), loopback " +
			"and link-local are excluded. A DNS record's content counts as a name pointing at " +
			"an address, not as an address this estate holds. What is behind each address is " +
			"one hop along the graph's request-path edges.",
		Caveat: "An inventory records that an address is published, not that anything listens " +
			"on it, and not whether a security list, firewall or WAF in front of it lets a " +
			"packet through. The list is also a floor rather than a total: an address appears " +
			"only where its provider reports one, and OCI compute instances report no VNIC " +
			"address at all, so an instance with a public IP is invisible here.",
		Rows: rows,
	}}
}

// ----------------------------------------------------------------------
// network.private-addresses
// ----------------------------------------------------------------------

type privateAddresses struct{}

func (privateAddresses) ID() string     { return "network.private-addresses" }
func (privateAddresses) Title() string  { return "Private address space in use" }
func (privateAddresses) Family() Family { return FamilyNetwork }

// fallbackBits is the block size used to bucket a private address that falls
// inside no declared range: a /16 for IPv4, a /48 for IPv6.
//
// The bucket is a presentation choice and is labelled as one in the rows. The
// inventory does not know the real prefix length of a range nobody declared —
// a Kubernetes pod network is the usual case — and inventing a /24 would imply
// a boundary that may not exist, while showing 300 individual addresses would
// bury the ranges that are known.
const (
	fallbackBitsV4 = 16
	fallbackBitsV6 = 48
)

func (x privateAddresses) Run(_ context.Context, in *Input) []Finding {
	ranges := declaredRanges(in)

	// occupants[prefix] is the set of addresses observed inside that block.
	occupants := map[netip.Prefix]map[netip.Addr]bool{}
	holders := map[netip.Prefix][]core.Asset{}
	// declared keeps every declarant of a block, not the first one. Two
	// networks declaring the identical range is common (a copied VCN
	// template), and a row naming only one of them would attribute the whole
	// block to the wrong network — the same mistake network.overlapping-cidrs
	// exists to point out.
	declared := map[netip.Prefix][]declaredRange{}
	for _, r := range ranges {
		if !privateAddr(r.prefix.Addr()) {
			continue
		}
		declared[r.prefix] = append(declared[r.prefix], r)
		if occupants[r.prefix] == nil {
			occupants[r.prefix] = map[netip.Addr]bool{}
		}
	}

	total := 0
	counted := map[string]bool{}
	for _, h := range heldAddresses(in) {
		if !privateAddr(h.addr) {
			continue
		}
		key := h.addr.String() + "\x00" + h.asset.Provider + "\x00" + h.asset.ID
		if counted[key] {
			continue
		}
		counted[key] = true
		total++

		bucket, ok := containingRange(ranges, h.addr)
		prefix := bucket.prefix
		if !ok || !privateAddr(prefix.Addr()) {
			bits := fallbackBitsV6
			if h.addr.Is4() {
				bits = fallbackBitsV4
			}
			p, err := h.addr.Prefix(bits)
			if err != nil {
				continue
			}
			prefix = p
		}
		if occupants[prefix] == nil {
			occupants[prefix] = map[netip.Addr]bool{}
		}
		occupants[prefix][h.addr] = true
		holders[prefix] = append(holders[prefix], h.asset)
	}

	if total == 0 && len(declared) == 0 {
		return nil
	}

	type block struct {
		prefix   netip.Prefix
		observed int
		declared bool
		label    string
	}
	blocks := make([]block, 0, len(occupants))
	for prefix, addrs := range occupants {
		b := block{prefix: prefix, observed: len(addrs)}
		if rs, ok := declared[prefix]; ok {
			b.declared = true
			b.label = declaredLabel(in, rs)
			if containsFinerBlock(declared, prefix) {
				// Addresses are attributed to the most specific block that
				// contains them, so a VCN's own row reads as empty while its
				// subnets hold everything. Saying so beats a reader concluding
				// the network is unused.
				b.label += "; finer blocks listed separately"
			}
		} else {
			b.label = fmt.Sprintf("no collected network declares this range; /%d bucket", prefix.Bits())
		}
		blocks = append(blocks, b)
	}
	// Declared blocks lead — they are the estate's address plan, and the
	// buckets below them are a residue. Within each group, the fullest first,
	// then by prefix so the order is total.
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if a.declared != b.declared {
			return a.declared
		}
		if a.observed != b.observed {
			return a.observed > b.observed
		}
		if c := a.prefix.Addr().Compare(b.prefix.Addr()); c != 0 {
			return c < 0
		}
		return a.prefix.Bits() < b.prefix.Bits()
	})

	rows := make([]Row, 0, len(blocks))
	for _, b := range blocks {
		rows = append(rows, Row{
			Label:   b.prefix.String(),
			Value:   occupancy(b.prefix, b.observed),
			Fact:    b.label,
			Related: refsOf(dedupeAssets(holders[b.prefix], 6)),
		})
	}

	declaredCount := 0
	for _, b := range blocks {
		if b.declared {
			declaredCount++
		}
	}

	return []Finding{{
		ID:       "network.private-addresses",
		Title:    "Private address space in use",
		Severity: SeverityInfo,
		Count:    total,
		Summary: fmt.Sprintf("%d private %s observed across %d address %s, %d of which a collected network declares.",
			total, pluralize(total, "address", "addresses"),
			len(blocks), pluralize(len(blocks), "block", "blocks"), declaredCount),
		Basis: "addresses that are RFC1918, IPv6 unique-local or CGNAT 100.64/10, bucketed into " +
			"the CIDR blocks providers declare (oci.subnet cidr_block, oci.vcn cidr_blocks, GCP " +
			"ip_cidr_range, netbird.route network) and, where no declared block contains them, " +
			"into a /16 (IPv4) or /48 (IPv6) so the shape is still visible. The occupancy column " +
			"is observed addresses over the size of the block.",
		Caveat: "Occupancy counts only the addresses that appear in this inventory, so every " +
			"figure is a floor and a block that looks empty may be full. An address is visible " +
			"only where a provider publishes one: OCI compute instances publish no VNIC address, " +
			"Kubernetes pod and node addresses need --include-raw, and no provider reports " +
			"addresses that are reserved, held by DHCP, or in use by anything outside the audit.",
		Rows: rows,
	}}
}

// declaredLabel names who declared a block: the resource and its network, or
// the several networks that each declare the identical range.
func declaredLabel(in *Input, rs []declaredRange) string {
	if len(rs) == 1 {
		name, network := DisplayName(rs[0].asset), networkName(in, rs[0])
		if name == network {
			return name
		}
		return name + " in " + network
	}
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, DisplayName(r.asset))
	}
	return fmt.Sprintf("declared by %d resources: %s", len(rs), strings.Join(names, ", "))
}

// containsFinerBlock reports whether some other declared block sits strictly
// inside this one.
func containsFinerBlock(declared map[netip.Prefix][]declaredRange, p netip.Prefix) bool {
	for other := range declared {
		if other != p && other.Bits() > p.Bits() && p.Contains(other.Addr()) {
			return true
		}
	}
	return false
}

// occupancy renders "observed / size" for a block, and just the count when the
// denominator is meaningless — an IPv6 block, or an IPv4 block bigger than a
// /16, where "12 / 16777216" is a rounding error dressed up as a measurement.
func occupancy(p netip.Prefix, observed int) string {
	if !p.Addr().Is4() || p.Bits() < 16 {
		return strconv.Itoa(observed)
	}
	size := 1 << uint(32-p.Bits())
	return fmt.Sprintf("%d / %d", observed, size)
}

// dedupeAssets keeps the first n distinct assets, preserving order.
func dedupeAssets(assets []core.Asset, n int) []core.Asset {
	seen := map[string]bool{}
	out := make([]core.Asset, 0, min(len(assets), n))
	for _, a := range assets {
		key := a.Provider + "\x00" + a.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		if out = append(out, a); len(out) == n {
			break
		}
	}
	return out
}

// ----------------------------------------------------------------------
// network.gateways
// ----------------------------------------------------------------------

type networkGateways struct{}

func (networkGateways) ID() string     { return "network.gateways" }
func (networkGateways) Title() string  { return "Network gateways" }
func (networkGateways) Family() Family { return FamilyNetwork }

// gatewayTypes are the OCI gateway objects, mapped to the short word a row
// prints. Kept in one table so the two findings below agree on what a gateway
// is.
//
// OCI only, deliberately: GCP publishes no gateway object of its own — Cloud
// NAT is configuration inside a Router rather than a resource Cloud Asset
// Inventory returns — so listing a Router here would put a thing that is not a
// gateway in a table that claims to count them.
var gatewayTypes = map[string]string{
	"oci.internet_gateway":      "internet",
	"oci.nat_gateway":           "NAT",
	"oci.service_gateway":       "service",
	"oci.local_peering_gateway": "peering",
}

// egressGatewayTypes are the subset the second finding asks about: a gateway
// that exists to give resources in a VCN a way out. A peering gateway is left
// out because it is half of a relationship with another VCN, so "nothing
// behind it in this VCN" is not even suspicious.
var egressGatewayTypes = map[string]bool{
	"oci.internet_gateway": true,
	"oci.nat_gateway":      true,
	"oci.service_gateway":  true,
}

func (networkGateways) Requires() Requirements {
	return Requirements{Types: []string{
		"oci.internet_gateway", "oci.nat_gateway", "oci.service_gateway",
		"oci.local_peering_gateway",
	}}
}

func (x networkGateways) Run(_ context.Context, in *Input) []Finding {
	var gateways []core.Asset
	for _, a := range in.Assets {
		if _, ok := gatewayTypes[a.Type]; ok {
			gateways = append(gateways, a)
		}
	}
	if len(gateways) == 0 {
		return nil
	}

	// Group by the network each gateway is attached to. A gateway whose vcn_id
	// names a VCN this audit did not collect keeps its own group rather than
	// being dropped — the gateway is real even where the VCN is out of scope.
	byNetwork := map[string][]core.Asset{}
	var networks []string
	for _, g := range gateways {
		key := g.Tags["vcn_id"]
		if key == "" {
			key = g.Provider + "/" + g.ID
		}
		if _, ok := byNetwork[key]; !ok {
			networks = append(networks, key)
		}
		byNetwork[key] = append(byNetwork[key], g)
	}

	perKind := map[string]int{}
	for _, g := range gateways {
		perKind[gatewayTypes[g.Type]]++
	}
	kinds := make([]string, 0, len(perKind))
	for k, n := range perKind {
		kinds = append(kinds, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(kinds)

	rows := make([]Row, 0, len(networks))
	for _, key := range networks {
		gws := byNetwork[key]
		label := key
		if vcn, ok := in.AssetByID(key); ok {
			label = DisplayName(vcn)
		} else if len(gws) == 1 {
			label = DisplayName(gws[0])
		}
		words := make([]string, 0, len(gws))
		for _, g := range gws {
			word := gatewayTypes[g.Type]
			if ip := g.Tags["nat_ip"]; ip != "" {
				word += " (" + ip + ")"
			}
			words = append(words, word)
		}
		sort.Strings(words)
		rows = append(rows, Row{
			Label:   label,
			Value:   strconv.Itoa(len(gws)),
			Fact:    strings.Join(words, ", "),
			Related: refsOf(gws),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := len(rows[i].Related), len(rows[j].Related); a != b {
			return a > b
		}
		return rows[i].Label < rows[j].Label
	})

	findings := []Finding{{
		ID:       "network.gateways",
		Title:    "Network gateways",
		Severity: SeverityInfo,
		Count:    len(gateways),
		Summary: fmt.Sprintf("%d network %s across %d %s: %s.",
			len(gateways), pluralize(len(gateways), "gateway", "gateways"),
			len(networks), pluralize(len(networks), "network", "networks"),
			strings.Join(kinds, ", ")),
		Basis: "the gateway objects providers collect (oci.internet_gateway, oci.nat_gateway, " +
			"oci.service_gateway, oci.local_peering_gateway), grouped by the vcn_id tag each " +
			"carries and resolved back to the VCN asset by id.",
		Caveat: "A gateway existing is not a route through it. Route tables, security lists and " +
			"DRG or VPN attachments are not collected, so a VCN with an internet gateway may " +
			"have nothing routed to it, and a VCN without one may still reach the internet " +
			"through a peered network. Nothing here counts a byte of traffic either.",
		Rows: rows,
	}}

	if f, ok := unusedEgressGateways(in, gateways); ok {
		findings = append(findings, f)
	}
	return findings
}

// unusedEgressGateways is the sharper — and much weaker — question: is there an
// egress gateway sitting in a network where this inventory records nothing that
// could be using it?
//
// The weakness is specific and is stated in the finding's own Caveat rather
// than only here: OCI compute instances record no VCN or subnet in this
// inventory, so the resources most likely to be behind a NAT gateway are
// exactly the ones this test cannot see.
func unusedEgressGateways(in *Input, gateways []core.Asset) (Finding, bool) {
	// Every identifier that stands for a network or a piece of one: the VCN's
	// own id and the ids of its subnets. An asset referencing any of them is
	// "in" the network.
	networkIDs := map[string]string{} // id → vcn key
	for _, g := range gateways {
		if vcn := g.Tags["vcn_id"]; vcn != "" {
			networkIDs[vcn] = vcn
		}
	}
	for _, s := range in.ByType("oci.subnet") {
		if vcn := s.Tags["vcn_id"]; vcn != "" && networkIDs[vcn] != "" {
			networkIDs[s.ID] = vcn
		}
	}
	if len(networkIDs) == 0 {
		return Finding{}, false
	}

	// A VCN is "occupied" when some asset that is not network plumbing points
	// at it or at one of its subnets through a tag. Scanning tag *values*
	// rather than a list of known keys means a provider that starts publishing
	// a subnet_id tomorrow is counted without a change here — and the ids being
	// matched are OCIDs, which nothing else collides with.
	occupied := map[string]bool{}
	for _, a := range in.Assets {
		if _, isGateway := gatewayTypes[a.Type]; isGateway {
			continue
		}
		switch a.Type {
		case "oci.vcn", "oci.subnet", "oci.compartment":
			continue
		}
		for _, v := range a.Tags {
			if vcn, ok := networkIDs[v]; ok {
				occupied[vcn] = true
			}
		}
	}

	var rows []Row
	networks := map[string]bool{}
	for _, g := range gateways {
		if !egressGatewayTypes[g.Type] {
			continue
		}
		vcn := g.Tags["vcn_id"]
		if vcn == "" || occupied[vcn] {
			continue
		}
		networks[vcn] = true
		name := vcn
		if a, ok := in.AssetByID(vcn); ok {
			name = DisplayName(a)
		}
		subnets := 0
		for id, key := range networkIDs {
			if key == vcn && id != vcn {
				subnets++
			}
		}
		rows = append(rows, Row{
			Label: DisplayName(g),
			Asset: refPtr(g),
			Fact: fmt.Sprintf("%s gateway in %s; %d collected %s, no resource in either",
				gatewayTypes[g.Type], name, subnets, pluralize(subnets, "subnet", "subnets")),
		})
	}
	if len(rows) == 0 {
		return Finding{}, false
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Label < rows[j].Label })

	return Finding{
		ID:       "network.gateway-without-workload",
		Title:    "Egress gateways in networks with nothing recorded behind them",
		Severity: SeverityNotable,
		Count:    len(rows),
		// "beyond the network's own subnets", not "no resource at all". The
		// occupancy scan above deliberately skips the VCN, its subnets and the
		// compartment — they constitute the network rather than occupy it — so
		// those assets are collected and the summary must not deny them. Each row
		// already says "N collected subnets, no resource in either"; a summary
		// claiming the stronger thing is contradicted by the table beneath it.
		Summary: fmt.Sprintf("%d egress %s in %d %s where this inventory records no resource "+
			"beyond the network's own subnets.",
			len(rows), pluralize(len(rows), "gateway sits", "gateways sit"),
			len(networks), pluralize(len(networks), "network", "networks")),
		Basis: "NAT, internet and service gateways whose vcn_id names a network that no " +
			"collected asset references — matched by scanning every other asset's tag values " +
			"for the VCN's OCID or one of its subnets' OCIDs.",
		Caveat: "This inference is weak and its blind spot is the common case: OCI compute " +
			"instances record no VCN or subnet in this inventory, so a network full of instances " +
			"looks empty to this test. Only resources that carry a vcn_id or subnet_id — OKE " +
			"clusters, network load balancers — can occupy a network here. It is also not a " +
			"bill: this project's own price book records the NAT, internet and service gateway " +
			"objects as carrying no charge, and what does cost money, the egress through them, " +
			"is traffic an inventory cannot see.",
		Rows: rows,
	}, true
}

func refPtr(a core.Asset) *core.AssetRef {
	ref := a.AsRef()
	return &ref
}

// ----------------------------------------------------------------------
// network.ingress-points
// ----------------------------------------------------------------------

type ingressPoints struct{}

func (ingressPoints) ID() string     { return "network.ingress-points" }
func (ingressPoints) Title() string  { return "Ingress points" }
func (ingressPoints) Family() Family { return FamilyNetwork }

// ingressTypes terminate a client connection: a rule set that answers on a
// hostname, or a cloud load balancer that answers on an address. Exact type
// strings, matching how internal/topology's resolvers select the same objects —
// a suffix match on ".Ingress" would also catch a CRD called Ingress in
// somebody else's API group.
var ingressTypes = map[string]bool{
	"networking.k8s.io/v1.Ingress":                true,
	"gateway.networking.k8s.io/v1.HTTPRoute":      true,
	"gateway.networking.k8s.io/v1beta1.HTTPRoute": true,
	"oci.load_balancer":                           true,
	"oci.network_load_balancer":                   true,
	"cloudflare.load_balancer":                    true,
	"compute.googleapis.com/ForwardingRule":       true,
}

func (ingressPoints) Requires() Requirements {
	return Requirements{Types: []string{
		"networking.k8s.io/v1.Ingress",
		"gateway.networking.k8s.io/v1.HTTPRoute",
		"gateway.networking.k8s.io/v1beta1.HTTPRoute",
		"v1.Service",
		"oci.load_balancer",
		"oci.network_load_balancer",
		"cloudflare.load_balancer",
		"compute.googleapis.com/ForwardingRule",
	}}
}

func (x ingressPoints) Run(_ context.Context, in *Input) []Finding {
	type point struct {
		asset     core.Asset
		hostnames []string
		addresses []netip.Addr
		backends  []core.Asset
		public    bool
	}

	var points []point
	for _, a := range in.Assets {
		if !ingressTypes[a.Type] && !isPublishedService(in, a) {
			continue
		}
		ad := topology.AssetAddresses(a)
		p := point{asset: a, hostnames: append([]string(nil), ad.Hostnames...)}
		p.hostnames = append(p.hostnames, routeHostnames(in, a)...)
		p.hostnames = append(p.hostnames, namesPointingAt(in, a)...)
		for _, s := range ad.IPs {
			if addr, ok := parseAddr(s); ok {
				p.addresses = append(p.addresses, addr)
				p.public = p.public || publicAddr(addr)
			}
		}
		p.backends = downstreamOf(in, a)
		p.hostnames = sortUnique(p.hostnames)
		points = append(points, p)
	}
	if len(points) == 0 {
		return nil
	}

	// The ones a name resolves to, and that front something, first: those are
	// the paths a request actually takes.
	sort.SliceStable(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if a.public != b.public {
			return a.public
		}
		if len(a.hostnames) != len(b.hostnames) {
			return len(a.hostnames) > len(b.hostnames)
		}
		if len(a.backends) != len(b.backends) {
			return len(a.backends) > len(b.backends)
		}
		return a.asset.ID < b.asset.ID
	})

	named, fronting := 0, 0
	rows := make([]Row, 0, len(points))
	for _, p := range points {
		if len(p.hostnames) > 0 {
			named++
		}
		if len(p.backends) > 0 {
			fronting++
		}

		where := shortType(p.asset)
		if ns := p.asset.Tags["namespace"]; ns != "" {
			where += " in " + ns
		}
		parts := []string{where}
		switch {
		case len(p.hostnames) > 0:
			parts = append(parts, strings.Join(p.hostnames[:min(len(p.hostnames), 2)], ", "))
		case len(p.addresses) > 0:
			parts = append(parts, p.addresses[0].String())
		}
		if len(p.backends) > 0 {
			parts = append(parts, "→ "+namesOf(p.backends, 2))
		} else {
			// Said explicitly rather than left blank: an ingress point with no
			// backend in the graph is either unresolved by this tool or
			// pointing at nothing, and a blank cell reads as neither.
			parts = append(parts, "no backend in the graph")
		}
		rows = append(rows, Row{
			Label:   DisplayName(p.asset),
			Asset:   refPtr(p.asset),
			Fact:    strings.Join(parts, "; "),
			Related: refsOf(p.backends),
		})
	}

	return []Finding{{
		ID:       "network.ingress-points",
		Title:    "Ingress points",
		Severity: SeverityInfo,
		Count:    len(points),
		Summary: fmt.Sprintf("%d ingress %s: %d answer on at least one hostname, %d have a backend in the graph.",
			len(points), pluralize(len(points), "point", "points"), named, fronting),
		Basis: "assets that terminate a client connection — Kubernetes Ingress and HTTPRoute, " +
			"Services carrying a load-balancer address, OCI load balancers and network load " +
			"balancers, Cloudflare load balancers, GCP forwarding rules — joined to their " +
			"hostnames (Ingress spec.rules[].host, HTTPRoute spec.hostnames, " +
			"status.loadBalancer.ingress[].hostname, plus any DNS record the graph resolves to " +
			"them) and to whatever the gateway-route, lb-backend and service-backend edges put " +
			"behind them.",
		Caveat: "A rule set lists the hostnames it would answer for, not that a controller ever " +
			"programmed it or that a client can reach it. What sits in front — TLS termination, " +
			"a WAF, an authenticating proxy — is not part of this join, and a backend the graph " +
			"could not resolve is absent here rather than reported: without --include-raw no " +
			"Kubernetes backend resolves at all.",
		Rows: rows,
	}}
}

// isPublishedService picks out the Services that are ingress points, which is
// not the same as the ones declaring spec.type: LoadBalancer. A Service becomes
// an entry point when a controller has actually given it an address, which is
// what status.loadBalancer.ingress records — the same evidence
// topology.EntryPoints uses, and stronger than a declared type that may still
// be pending.
func isPublishedService(in *Input, a core.Asset) bool {
	if a.Type != "v1.Service" {
		return false
	}
	ad := topology.AssetAddresses(a)
	return len(ad.IPs) > 0 || len(ad.Hostnames) > 0
}

// routeHostnames reads an HTTPRoute's spec.hostnames. The topology index reads
// an Ingress's spec.rules[].host but not this — adding it there would resolve
// CNAMEs onto HTTPRoutes and change the graph, so it is read here instead.
func routeHostnames(in *Input, a core.Asset) []string {
	if !strings.HasSuffix(a.Type, ".HTTPRoute") {
		return nil
	}
	v, ok := in.RawPath(a, "spec.hostnames")
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, h := range list {
		if s, ok := h.(string); ok && s != "" {
			out = append(out, strings.ToLower(s))
		}
	}
	return out
}

func sortUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------------------
// network.dangling-dns
// ----------------------------------------------------------------------

type danglingDNS struct{}

func (danglingDNS) ID() string     { return "network.dangling-dns" }
func (danglingDNS) Title() string  { return "DNS records pointing into this estate at nothing" }
func (danglingDNS) Family() Family { return FamilyNetwork }

func (danglingDNS) Requires() Requirements {
	// Topology, because the primary test is "the graph resolved this record to
	// nothing" and a graph with no edges resolves everything to nothing — it
	// would report every record in the audit as dangling.
	return Requirements{Topology: true, Types: []string{"cloudflare.dns_record"}}
}

func (x danglingDNS) Run(_ context.Context, in *Input) []Finding {
	records := in.ByType("cloudflare.dns_record")
	if len(records) == 0 {
		return nil
	}

	ranges := declaredRanges(in)
	owned := ownedHostnames(in)
	wildcards := wildcardHostnames(records)
	zones := collectedZones(in)

	held := map[netip.Addr]bool{}
	for _, h := range heldAddresses(in) {
		held[h.addr] = true
	}

	type suspect struct {
		record core.Asset
		fact   string
		tier   int // 1: inside a zone we hold, 2: inside a range we declare
		// evidence is the asset the tier rests on — the zone, or the subnet
		// whose CIDR contains the target — so a reader can check the claim
		// without the row having to spell an id into prose.
		evidence []core.AssetRef
	}
	var suspects []suspect
	outOfScope := 0

	for _, rec := range records {
		content := strings.TrimSpace(rec.Tags["content"])
		if content == "" || resolvedByGraph(in, rec) {
			continue
		}
		switch rec.Tags["type"] {
		case "A", "AAAA":
			addr, ok := parseAddr(content)
			if !ok || held[addr] {
				continue
			}
			block, inScope := containingRange(ranges, addr)
			if !inScope {
				outOfScope++
				continue
			}
			suspects = append(suspects, suspect{
				record: rec, tier: 2,
				fact: fmt.Sprintf("%s → %s, inside %s and held by nothing",
					rec.Tags["type"], content, block.prefix),
				evidence: []core.AssetRef{block.asset.AsRef()},
			})

		case "CNAME":
			target := normalizeHostname(content)
			if owned[target] || coveredByWildcard(wildcards, target) {
				continue
			}
			zone, inScope := zoneOf(zones, target)
			if !inScope {
				outOfScope++
				continue
			}
			suspects = append(suspects, suspect{
				record: rec, tier: 1,
				fact: fmt.Sprintf("CNAME → %s, a name in %s that nothing answers for", content, zone),
			})
		}
		// Other record types (TXT, MX, SRV, NS) are skipped rather than
		// reported: no resolver models them, so every one of them would look
		// unresolved, and a report where every MX record is a finding is a
		// report that gets switched off.
	}

	if len(suspects) == 0 {
		return nil
	}
	sort.SliceStable(suspects, func(i, j int) bool {
		if suspects[i].tier != suspects[j].tier {
			return suspects[i].tier < suspects[j].tier
		}
		return DisplayName(suspects[i].record) < DisplayName(suspects[j].record)
	})

	rows := make([]Row, 0, len(suspects))
	for _, s := range suspects {
		row := AssetRow(s.record, s.fact)
		row.Related = s.evidence
		if zoneID := s.record.Tags["zone_id"]; zoneID != "" {
			if zone, ok := in.AssetByID(zoneID); ok {
				row.Related = append(row.Related, zone.AsRef())
			}
		}
		rows = append(rows, row)
	}

	// The count of what was deliberately left out is part of the caveat, not a
	// footnote: it is the number that says how much this finding is choosing
	// not to claim.
	unlisted := "No other record resolved to nothing here, and the test is restricted to " +
		"targets inside a range or zone this audit describes because a target belonging to a " +
		"service nobody audited is indistinguishable from one that was deleted"
	if outOfScope > 0 {
		unlisted = fmt.Sprintf(pluralize(outOfScope,
			"%d further record here resolves to nothing but points outside every range and zone "+
				"this audit describes, and is not listed because a target belonging to a service "+
				"nobody audited is indistinguishable from one that was deleted",
			"%d further records here resolve to nothing but point outside every range and zone "+
				"this audit describes, and are not listed because a target belonging to a service "+
				"nobody audited is indistinguishable from one that was deleted"), outOfScope)
	}
	caveat := fmt.Sprintf("A record only looks dangling when the thing it points at was "+
		"collected. %s. The rows below can be wrong the same way: OCI compute instances publish "+
		"no address at all in this inventory, so a record aimed at an instance inside your own "+
		"VCN appears here. Nothing was resolved over the network — this is a join against "+
		"collected records, not a lookup.", unlisted)
	if !in.Scope.RawAvailable() {
		caveat += " This snapshot carries no Raw payloads, so no Kubernetes address or " +
			"hostname was visible to the join at all; treat every row as unverified."
	}

	return []Finding{{
		ID:       "network.dangling-dns",
		Title:    "DNS records pointing into this estate at nothing",
		Severity: SeverityWarn,
		Count:    len(suspects),
		Summary: fmt.Sprintf("%d DNS %s into address space or a zone this audit holds, and nothing in it answers there.",
			len(suspects), pluralize(len(suspects), "record points", "records point")),
		Basis: "cloudflare.dns_record entries of type A, AAAA or CNAME that the topology graph " +
			"resolved to no asset and whose target no collected asset holds, kept only when the " +
			"target is inside a CIDR the inventory declares (oci.vcn, oci.subnet, GCP " +
			"ip_cidr_range, netbird.route) or inside a Cloudflare zone this audit collected. " +
			"Wildcard records in the same zone count as answering.",
		Caveat: caveat,
		Rows:   rows,
	}}
}

// resolvedByGraph reports whether the topology graph joined this record to
// something. dnsToTarget deliberately does not join an A record to a sibling
// record sharing its address, so "resolved" here means an actual endpoint was
// found, which is the question.
func resolvedByGraph(in *Input, rec core.Asset) bool {
	for _, e := range in.EdgesFrom(rec.AsRef()) {
		if e.Kind == core.EdgeKindDNS {
			return true
		}
	}
	return false
}

// ownedHostnames is every name something in the inventory answers on. A second
// opinion rather than the primary test — the graph's hostname index already
// covers these — kept because it costs one map and it is the check that does
// not depend on an edge having been drawn.
func ownedHostnames(in *Input) map[string]bool {
	out := map[string]bool{}
	for _, a := range in.Assets {
		for _, h := range topology.AssetAddresses(a).Hostnames {
			out[h] = true
		}
	}
	return out
}

// wildcardHostnames collects the suffixes of wildcard records ("*.example.com"
// → "example.com").
//
// Matching them is deliberately loose — any name under the suffix counts as
// answered, where DNS only matches one label — because over-matching here
// *suppresses* a finding. Being quiet about a record that might be fine is the
// safe direction; claiming a takeover that isn't there is not.
func wildcardHostnames(records []core.Asset) []string {
	var out []string
	for _, r := range records {
		name := normalizeHostname(r.Name)
		if strings.HasPrefix(name, "*.") {
			out = append(out, strings.TrimPrefix(name, "*."))
		}
	}
	return out
}

func coveredByWildcard(wildcards []string, host string) bool {
	for _, w := range wildcards {
		if host == w || strings.HasSuffix(host, "."+w) {
			return true
		}
	}
	return false
}

// collectedZones is the DNS zones this audit holds, from the zone assets
// themselves and from the zone_name tag every zone-scoped record carries.
// Both, because either can be missing: a token scoped to DNS reads records
// without listing zones.
func collectedZones(in *Input) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = normalizeHostname(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, z := range in.ByType("cloudflare.zone") {
		add(z.Name)
	}
	for _, r := range in.ByType("cloudflare.dns_record") {
		add(r.Tags["zone_name"])
	}
	sort.Strings(out)
	return out
}

// zoneOf returns the most specific collected zone a hostname belongs to.
func zoneOf(zones []string, host string) (string, bool) {
	best := ""
	for _, z := range zones {
		if host != z && !strings.HasSuffix(host, "."+z) {
			continue
		}
		if len(z) > len(best) {
			best = z
		}
	}
	return best, best != ""
}

// normalizeHostname matches how topology normalizes a name for its index:
// lower-cased, trailing dot stripped. Kept in step with it so a lookup against
// ownedHostnames cannot miss on punctuation.
func normalizeHostname(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// ----------------------------------------------------------------------
// network.overlapping-cidrs
// ----------------------------------------------------------------------

type overlappingCIDRs struct{}

func (overlappingCIDRs) ID() string     { return "network.overlapping-cidrs" }
func (overlappingCIDRs) Title() string  { return "Networks with overlapping address ranges" }
func (overlappingCIDRs) Family() Family { return FamilyNetwork }

func (overlappingCIDRs) Requires() Requirements {
	return Requirements{Types: []string{
		"oci.vcn", "oci.subnet", "compute.googleapis.com/Subnetwork",
	}}
}

func (x overlappingCIDRs) Run(_ context.Context, in *Input) []Finding {
	ranges := declaredRanges(in)

	type pair struct {
		label   string
		fact    string
		count   int
		related []core.AssetRef
	}
	pairs := map[string]*pair{}
	var order []string

	for i := 0; i < len(ranges); i++ {
		for j := i + 1; j < len(ranges); j++ {
			a, b := ranges[i], ranges[j]
			// Same network: a VCN contains its own subnets, and two subnets of
			// one VCN cannot overlap. Comparing them would report every estate.
			if a.network == b.network || !a.prefix.Overlaps(b.prefix) {
				continue
			}
			// Order the pair by network key so "prod ↔ stage" and "stage ↔
			// prod" are one row. Without this a VCN pair produces a row per
			// direction, and which direction depends on the order two subnets
			// happened to sort in.
			if a.network > b.network {
				a, b = b, a
			}
			key := a.network + "\x00" + b.network
			p, ok := pairs[key]
			if !ok {
				p = &pair{
					label:   fmt.Sprintf("%s ↔ %s", networkName(in, a), networkName(in, b)),
					fact:    fmt.Sprintf("%s overlaps %s", a.prefix, b.prefix),
					related: []core.AssetRef{a.asset.AsRef(), b.asset.AsRef()},
				}
				pairs[key] = p
				order = append(order, key)
			}
			p.count++
		}
	}

	rows := make([]Row, 0, len(order))
	for _, key := range order {
		p := pairs[key]
		fact := p.fact
		if p.count > 1 {
			fact += fmt.Sprintf(" (and %d more %s)", p.count-1, pluralize(p.count-1, "pair", "pairs"))
		}
		rows = append(rows, Row{
			Label:   p.label,
			Value:   strconv.Itoa(p.count),
			Fact:    fact,
			Related: p.related,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Label < rows[j].Label })

	// The one exact signal available here: a local peering gateway records what
	// its peer advertises, so a peer advertising a block that overlaps this
	// VCN's own is a collision the provider itself is reporting — not an
	// inference from two independent CIDR declarations.
	peerRows := peeringCollisions(in, ranges)
	rows = append(peerRows, rows...)

	if len(rows) == 0 {
		return nil
	}
	return []Finding{{
		ID:       "network.overlapping-cidrs",
		Title:    "Networks with overlapping address ranges",
		Severity: SeverityNotable,
		Count:    len(rows),
		Summary: fmt.Sprintf("%d %s of networks declare address ranges that overlap.",
			len(rows), pluralize(len(rows), "pair", "pairs")),
		Basis: "CIDR blocks declared by oci.vcn (cidr_blocks), oci.subnet (cidr_block), GCP " +
			"Subnetworks (ip_cidr_range) and netbird.route (network), compared pairwise with " +
			"netip.Prefix.Overlaps and grouped by the network each block belongs to, so blocks " +
			"inside one VCN are never compared. Rows marked as a peer advertisement come from " +
			"a local peering gateway's own peer_advertised_cidr.",
		Caveat: "Overlapping ranges only matter where the two networks are joined, and routing " +
			"is not collected: route tables, DRG and VPN attachments and peering state beyond " +
			"the gateway objects are all invisible here, so a pair listed below may be " +
			"deliberately isolated and a pair not listed may still collide over a route this " +
			"tool cannot see. Networks whose provider publishes no range at all cannot appear.",
		Rows: rows,
	}}
}

// peeringCollisions reports a VCN whose peer advertises a range overlapping its
// own — the provider's own field saying the two cannot both be routed.
func peeringCollisions(in *Input, ranges []declaredRange) []Row {
	var rows []Row
	for _, lpg := range in.ByType("oci.local_peering_gateway") {
		advertised := strings.TrimSpace(lpg.Tags["peer_advertised_cidr"])
		if advertised == "" {
			continue
		}
		peer, err := netip.ParsePrefix(advertised)
		if err != nil {
			continue
		}
		// The least specific overlapping block of this VCN: the VCN's own CIDR
		// where it declares one, rather than whichever subnet happened to sort
		// first. "your peer advertises the block your VCN is" is the sentence
		// worth printing.
		vcnID := lpg.Tags["vcn_id"]
		var hit declaredRange
		found := false
		for _, r := range ranges {
			if r.network != vcnID || !r.prefix.Overlaps(peer.Masked()) {
				continue
			}
			if !found || r.prefix.Bits() < hit.prefix.Bits() {
				hit, found = r, true
			}
		}
		if !found {
			continue
		}
		name := vcnID
		if vcn, ok := in.AssetByID(vcnID); ok {
			name = DisplayName(vcn)
		}
		rows = append(rows, Row{
			Label: fmt.Sprintf("%s ↔ peer of %s", name, DisplayName(lpg)),
			Asset: refPtr(lpg),
			Fact: fmt.Sprintf("peer advertises %s, overlapping %s (%s)",
				peer, hit.prefix, strings.ToLower(lpg.Tags["peering_status"])),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Label < rows[j].Label })
	return rows
}
