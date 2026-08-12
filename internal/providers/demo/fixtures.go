package demo

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// The fixture models "Northwind", a fictional retailer running an OCI tenancy
// fronted by Cloudflare, two OKE clusters, a GCP project, and two overlay
// networks. It is shaped backwards from the topology resolvers: every asset
// that carries an IP, a hostname, an OCID tag, or a policy selector does so
// because some resolver joins on it. Changing an address or a label here can
// silently delete an edge — internal/providers/demo/demo_test.go asserts that
// at least one edge of every core.EdgeKind survives.
//
// Everything is deterministic: fixed identifiers, a fixed epoch, and an
// xorshift PRNG seeded per section for the filler. No time.Now, no map
// iteration order leaking into output.

// fixtureEpoch anchors every CreatedAt. Assets offset from it by a fixed
// number of hours, so timestamps look plausible and never move between runs.
const fixtureEpochRFC3339 = "2024-01-08T09:00:00Z"

// Addresses deliberately shared across provider sections — these are the
// joins. All are from the RFC 5737 / RFC 3849 documentation ranges except the
// CGNAT-space overlay addresses, which are what Tailscale and NetBird really
// hand out.
const (
	ociProdLBIP  = "203.0.113.10" // OCI prod LB → also the K8s edge Service's external IP
	ociStageLBIP = "203.0.113.11"
	ociPlatLBIP  = "203.0.113.12"
	gcpFwdRuleIP = "198.51.100.20" // GCP forwarding rule, targeted by a CF A record

	kubeStageLBHost = "nw-stage.lb.eu-frankfurt-1.oci.example" // Service LB hostname → CF CNAME target
	tsBastionDNS    = "bastion.tailfe8c.ts.net"
	tsBastionIP     = "100.64.0.9"
	nbGatewayDNS    = "nb-gateway.netbird.selfhosted"
	nbGatewayIP     = "100.92.14.6"
)

// Cloudflare account and zone identifiers. Zone ids are referenced by the
// dns/ruleset/access/page-rule sections, where they become the zone_id tag
// the wafBinding resolver joins on.
const (
	cfAccountProd = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	cfAccountLabs = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	cfZoneExample = "023e105f4ecef8ad9ca31a8372d0c353"
	cfZoneIO      = "9b7c1e4d2a6f48c0b3d5e7f9a1c3b5d7"
	cfZoneLabs    = "4c8e2f6a0b9d47e1a3f5c7b9d1e3f5a7"
)

// OCI tenancy + compartment OCIDs. The compartment tree is
// root → platform → {prod, staging}.
const (
	ociTenancy         = "ocid1.tenancy.oc1..aaaaaaaanorthwindroot0001"
	ociCompartmentPlat = "ocid1.compartment.oc1..aaaaaaaanwplatform0001"
	ociCompartmentProd = "ocid1.compartment.oc1..aaaaaaaanwprod00000001"
	ociCompartmentStg  = "ocid1.compartment.oc1..aaaaaaaanwstaging00001"
)

// Kubernetes cluster identities. The real provider uses the kube-system
// namespace UID as AccountID; these are shaped the same way.
const (
	kubeProdCluster  = "6f1a0c33-9d2e-4b7a-8c15-2e5b7d9f0a41"
	kubeStageCluster = "b28d5e91-4c67-4a03-9f2d-71ac36e08b52"
)

const (
	gcpProject       = "northwind-prod-4417"
	tailnetName      = "northwind.example.com"
	netbirdAccountID = "d7f3a1c9e05b4826"
)

// Assets returns the complete synthetic inventory. Every call rebuilds the
// slice so callers can mutate what they get (Collect strips Raw in place).
func Assets() []core.Asset {
	out := make([]core.Asset, 0, 640)
	out = append(out, cloudflareAssets()...)
	out = append(out, ociAssets()...)
	out = append(out, kubernetesAssets()...)
	out = append(out, gcpAssets()...)
	out = append(out, tailscaleAssets()...)
	out = append(out, netbirdAssets()...)
	return out
}

// ----------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------

var fixtureEpoch = mustParseTime(fixtureEpochRFC3339)

func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("demo: bad fixture timestamp " + s)
	}
	return t
}

// created returns a *time.Time offsetHours after the epoch. Deriving from a
// fixed epoch keeps the fixture readable without hard-coding 600 literals,
// and is just as reproducible.
func created(offsetHours int) *time.Time {
	t := fixtureEpoch.Add(time.Duration(offsetHours) * time.Hour)
	return &t
}

// tags builds a tag map from key/value pairs, dropping empty values so an
// absent field looks absent rather than blank (matching the real providers).
func tags(kv ...string) map[string]string {
	if len(kv)%2 != 0 {
		panic("demo: tags called with an odd number of arguments")
	}
	out := make(map[string]string, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		out[kv[i]] = kv[i+1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// raw marshals a fixture payload for Asset.Raw. Go sorts map keys when
// marshalling, so a map[string]any literal serialises identically every run.
func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// uid synthesises a UUID-shaped, content-derived identifier — what Kubernetes
// object UIDs look like, without needing 200 literals or a random source.
func uid(parts ...string) string {
	key := strings.Join(parts, "/")
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(key))
	h2 := fnv.New64a()
	_, _ = h2.Write([]byte("northwind:" + key))
	a, b := h1.Sum64(), h2.Sum64()
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x",
		uint32(a>>32), uint16(a>>16), uint16(a)&0x0fff,
		uint16(b>>48)&0x3fff|0x8000, b&0xffffffffffff)
}

// rng is a xorshift64 PRNG. It exists only to give the filler assets varied
// shapes/regions/sizes; every section seeds it with a constant, so the output
// is fixed.
type rng struct{ s uint64 }

func newRNG(seed uint64) *rng {
	if seed == 0 {
		seed = 88172645463325252
	}
	return &rng{s: seed}
}

func (r *rng) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

func (r *rng) n(max int) int {
	if max <= 0 {
		return 0
	}
	return int(r.next() % uint64(max))
}

func (r *rng) pick(xs []string) string { return xs[r.n(len(xs))] }

func itoa(n int) string { return strconv.Itoa(n) }

// pad renders a zero-padded ordinal for generated names/ids.
func pad(n int) string { return fmt.Sprintf("%04d", n) }

// ----------------------------------------------------------------------
// Cloudflare
// ----------------------------------------------------------------------

type cfZone struct {
	id, name, account, plan string
}

var cfZones = []cfZone{
	{cfZoneExample, "northwind.example", cfAccountProd, "Enterprise"},
	{cfZoneIO, "northwind.io", cfAccountProd, "Business"},
	{cfZoneLabs, "nwlabs.dev", cfAccountLabs, "Pro"},
}

func cloudflareAssets() []core.Asset {
	out := make([]core.Asset, 0, 180)

	out = append(out,
		cfAsset("cloudflare.account", cfAccountProd, "Northwind Production", cfAccountProd, "active", 0,
			tags("type", "enterprise", "settings_enforce_twofactor", "true"),
			map[string]any{"id": cfAccountProd, "name": "Northwind Production"}),
		cfAsset("cloudflare.account", cfAccountLabs, "Northwind Labs", cfAccountLabs, "active", 2,
			tags("type", "standard", "settings_enforce_twofactor", "false"),
			map[string]any{"id": cfAccountLabs, "name": "Northwind Labs"}),
	)

	for i, z := range cfZones {
		out = append(out, cfAsset("cloudflare.zone", z.id, z.name, z.account, "active", 4+i,
			tags("plan", z.plan, "account_name", "Northwind", "name_servers", "ada.ns.cloudflare.com,bob.ns.cloudflare.com"),
			map[string]any{"id": z.id, "name": z.name, "status": "active", "plan": map[string]any{"name": z.plan}}))
	}

	out = append(out, cfDNSRecords()...)
	out = append(out, cfSecurityAssets()...)
	out = append(out, cfPlatformAssets()...)
	return out
}

func cfAsset(typ, id, name, account, status string, hourOffset int, tg map[string]string, rawPayload any) core.Asset {
	return core.Asset{
		Provider:  "cloudflare",
		AccountID: account,
		Type:      typ,
		ID:        id,
		Name:      name,
		Status:    status,
		CreatedAt: created(hourOffset),
		Tags:      tg,
		Raw:       raw(rawPayload),
	}
}

// cfDNSRecord is one record in the fixture. content is what dnsToTarget joins
// on, so the anchor records below point at addresses/hostnames that other
// sections really publish.
type cfDNSRecord struct {
	zone           cfZone
	name, typ, val string
	proxied        bool
}

// cfAnchorDNS are the records that produce topology edges. Each comment names
// the asset on the other end.
func cfAnchorDNS() []cfDNSRecord {
	z0, z1, z2 := cfZones[0], cfZones[1], cfZones[2]
	return []cfDNSRecord{
		{z0, "northwind.example", "A", ociProdLBIP, true},                   // → OCI prod LB + K8s edge Service
		{z0, "www.northwind.example", "A", ociProdLBIP, true},               // → same
		{z0, "api.northwind.example", "A", ociProdLBIP, true},               // → same
		{z0, "shop.northwind.example", "A", gcpFwdRuleIP, true},             // → GCP forwarding rule
		{z0, "cdn.northwind.example", "A", gcpFwdRuleIP, true},              // → GCP forwarding rule
		{z0, "db.northwind.example", "A", ociStageLBIP, false},              // → OCI staging LB
		{z0, "bastion.northwind.example", "CNAME", tsBastionDNS, false},     // → Tailscale device
		{z1, "stage.northwind.io", "CNAME", kubeStageLBHost, true},          // → K8s stage Service (LB hostname)
		{z1, "lb.northwind.io", "A", ociPlatLBIP, false},                    // → OCI platform LB
		{z1, "mesh.northwind.io", "CNAME", nbGatewayDNS, false},             // → NetBird gateway peer
		{z1, "assets.northwind.io", "CNAME", "www.northwind.example", true}, // → sibling record chain
		{z2, "vpn.nwlabs.dev", "A", tsBastionIP, false},                     // → Tailscale device (overlay IP)
		{z2, "nb.nwlabs.dev", "A", nbGatewayIP, false},                      // → NetBird gateway peer (overlay IP)
		{z2, "edge.nwlabs.dev", "A", ociStageLBIP, true},                    // → OCI staging LB
	}
}

func cfDNSRecords() []core.Asset {
	recs := cfAnchorDNS()

	// Filler records give the Assets table and facet counts a realistic
	// shape. Their contents sit in TEST-NET-1 / documentation ranges that no
	// other asset publishes, so they add volume without inventing edges.
	r := newRNG(0x5eed01)
	prefixes := []string{"svc", "app", "worker", "mail", "img", "static", "int", "vpn", "ops", "metrics"}
	for i := 0; i < 106; i++ {
		z := cfZones[i%len(cfZones)]
		host := fmt.Sprintf("%s-%s.%s", r.pick(prefixes), pad(i), z.name)
		switch {
		case i%17 == 3:
			// Filler CNAMEs point at distinct off-inventory hostnames. Aiming
			// them all at one name would bucket them together in the hostname
			// index and, because a CNAME→CNAME match is a real resolution
			// chain, mesh every filler record to every other one — dozens of
			// meaningless edges. The single deliberate chain is the
			// assets.northwind.io anchor above.
			recs = append(recs, cfDNSRecord{z, host, "CNAME", fmt.Sprintf("pop-%s.northwind-edge.example", pad(i)), true})
		case i%7 == 0:
			recs = append(recs, cfDNSRecord{z, host, "AAAA", fmt.Sprintf("2001:db8:4a::%x", 0x100+i), false})
		case i%11 == 0:
			recs = append(recs, cfDNSRecord{z, "_dmarc." + z.name + "." + pad(i), "TXT", "v=DMARC1; p=quarantine", false})
		case i%13 == 0:
			recs = append(recs, cfDNSRecord{z, host, "MX", "10 mx" + itoa(i%3+1) + ".mail.example", false})
		default:
			recs = append(recs, cfDNSRecord{z, host, "A", fmt.Sprintf("192.0.2.%d", i%253+1), i%2 == 0})
		}
	}

	out := make([]core.Asset, 0, len(recs))
	for i, rec := range recs {
		id := fmt.Sprintf("dns%s%s", rec.zone.id[:8], pad(i))
		out = append(out, core.Asset{
			Provider:  "cloudflare",
			AccountID: rec.zone.account,
			Type:      "cloudflare.dns_record",
			ID:        id,
			Name:      rec.name,
			CreatedAt: created(24 + i),
			Tags: tags(
				"zone_id", rec.zone.id,
				"zone_name", rec.zone.name,
				"type", rec.typ,
				"content", rec.val,
				"proxied", strconv.FormatBool(rec.proxied),
				"ttl", "1",
			),
			Raw: raw(map[string]any{
				"id": id, "name": rec.name, "type": rec.typ,
				"content": rec.val, "proxied": rec.proxied, "ttl": 1,
				"zone_id": rec.zone.id, "zone_name": rec.zone.name,
			}),
		})
	}
	return out
}

// cfSecurityAssets are the zone-bound resources the wafBinding resolver joins
// to their zone through the zone_id tag.
func cfSecurityAssets() []core.Asset {
	out := make([]core.Asset, 0, 24)

	for i, z := range cfZones {
		for j, phase := range []string{"http_request_firewall_managed", "http_ratelimit"} {
			id := fmt.Sprintf("rs%s%d%d", z.id[:10], i, j)
			out = append(out, core.Asset{
				Provider:  "cloudflare",
				AccountID: z.account,
				Type:      "cloudflare.ruleset",
				ID:        id,
				Name:      fmt.Sprintf("Northwind %s (%s)", phase, z.name),
				Status:    "deployed",
				CreatedAt: created(160 + i*4 + j),
				Tags: tags(
					"zone_id", z.id,
					"zone_name", z.name,
					"scope", "zone",
					"phase", phase,
					"kind", "zone",
					"rules_count", itoa(4+j*3),
				),
				Raw: raw(map[string]any{"id": id, "phase": phase, "kind": "zone", "rules": []any{}}),
			})
		}
	}

	// One account-scoped managed ruleset — the "same id at two scopes"
	// case the real collector warns about, discriminated by the scope tag.
	out = append(out, core.Asset{
		Provider: "cloudflare", AccountID: cfAccountProd,
		Type: "cloudflare.ruleset", ID: "rsacct000northwindmanaged",
		Name: "Cloudflare Managed Ruleset (account)", Status: "deployed",
		CreatedAt: created(150),
		Tags:      tags("scope", "account", "phase", "http_request_firewall_managed", "kind", "managed", "rules_count", "212"),
		Raw:       raw(map[string]any{"id": "rsacct000northwindmanaged", "kind": "managed"}),
	})

	accessApps := []struct {
		zone     cfZone
		name     string
		domain   string
		sessDur  string
		policies int
	}{
		{cfZones[0], "Northwind Admin", "admin.northwind.example", "24h", 3},
		{cfZones[0], "Grafana", "grafana.northwind.example", "8h", 2},
		{cfZones[1], "Staging Console", "console.northwind.io", "12h", 2},
		{cfZones[2], "Labs Notebook", "notebook.nwlabs.dev", "1h", 1},
	}
	for i, app := range accessApps {
		id := fmt.Sprintf("acc%s%s", app.zone.id[:8], pad(i))
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: app.zone.account,
			Type: "cloudflare.access_app", ID: id, Name: app.name, Status: "active",
			CreatedAt: created(180 + i*3),
			Tags: tags(
				"zone_id", app.zone.id,
				"zone_name", app.zone.name,
				"domain", app.domain,
				"session_duration", app.sessDur,
				"policies", itoa(app.policies),
				"type", "self_hosted",
			),
			Raw: raw(map[string]any{"id": id, "name": app.name, "domain": app.domain, "type": "self_hosted"}),
		})
	}

	pageRules := []struct {
		zone   cfZone
		target string
	}{
		{cfZones[0], "*northwind.example/static/*"},
		{cfZones[0], "*northwind.example/api/*"},
		{cfZones[1], "*northwind.io/*"},
	}
	for i, pr := range pageRules {
		id := fmt.Sprintf("pr%s%s", pr.zone.id[:10], pad(i))
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: pr.zone.account,
			Type: "cloudflare.page_rule", ID: id, Name: pr.target, Status: "active",
			CreatedAt: created(190 + i),
			Tags: tags(
				"zone_id", pr.zone.id,
				"zone_name", pr.zone.name,
				"target", pr.target,
				"priority", itoa(i+1),
				"actions", "cache_level,edge_cache_ttl",
			),
			Raw: raw(map[string]any{"id": id, "targets": []any{pr.target}, "priority": i + 1}),
		})
	}

	// Tunnels are account-scoped and carry no zone_id — they are in the
	// wafBinding candidate list and correctly never match. Kept so the demo
	// reflects that reality rather than tidying it away.
	tunnels := []string{"nw-prod-egress", "nw-stage-egress", "nw-lab-egress"}
	for i, name := range tunnels {
		acct := cfAccountProd
		if i == 2 {
			acct = cfAccountLabs
		}
		id := uid("cf", "tunnel", name)
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: acct,
			Type: "cloudflare.tunnel", ID: id, Name: name, Status: "healthy",
			CreatedAt: created(200 + i*2),
			Tags:      tags("connections", itoa(2+i), "tun_type", "cfd_tunnel"),
			Raw:       raw(map[string]any{"id": id, "name": name, "status": "healthy"}),
		})
	}

	certs := []struct {
		typ, name, zone, acct, status string
	}{
		{"cloudflare.certificate_pack", "northwind.example", cfZoneExample, cfAccountProd, "active"},
		{"cloudflare.certificate_pack", "northwind.io", cfZoneIO, cfAccountProd, "active"},
		{"cloudflare.certificate_pack", "nwlabs.dev", cfZoneLabs, cfAccountLabs, "pending_validation"},
		{"cloudflare.custom_certificate", "northwind.example (custom)", cfZoneExample, cfAccountProd, "active"},
		{"cloudflare.mtls_certificate", "Northwind device CA", "", cfAccountProd, "active"},
		{"cloudflare.mtls_certificate", "Partner API CA", "", cfAccountProd, "active"},
	}
	for i, c := range certs {
		id := uid("cf", "cert", c.typ, c.name)
		zoneName := ""
		for _, z := range cfZones {
			if z.id == c.zone {
				zoneName = z.name
			}
		}
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: c.acct,
			Type: c.typ, ID: id, Name: c.name, Status: c.status,
			CreatedAt: created(210 + i),
			Tags: tags(
				"zone_id", c.zone,
				"zone_name", zoneName,
				"issuer", "DigiCert Inc",
				"validity_days", "90",
				"signature", "ECDSAWithSHA256",
			),
			Raw: raw(map[string]any{"id": id, "issuer": "DigiCert Inc", "status": c.status}),
		})
	}

	return out
}

// cfPlatformAssets are the account-scoped developer-platform resources —
// no topology edges, plenty of inventory texture.
func cfPlatformAssets() []core.Asset {
	out := make([]core.Asset, 0, 24)
	r := newRNG(0x5eed02)

	buckets := []string{"nw-product-images", "nw-invoices", "nw-backups", "nw-analytics-raw", "nw-lab-scratch", "nw-terraform-state"}
	for i, b := range buckets {
		acct := cfAccountProd
		if i >= 4 {
			acct = cfAccountLabs
		}
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: acct, Region: r.pick([]string{"WEUR", "ENAM", "APAC"}),
			Type: "cloudflare.r2_bucket", ID: "r2-" + b, Name: b, Status: "active",
			CreatedAt: created(230 + i*3),
			Tags:      tags("storage_class", "Standard"),
			Raw:       raw(map[string]any{"name": b, "storage_class": "Standard"}),
		})
	}

	workers := []string{"nw-edge-router", "nw-image-resize", "nw-ab-test", "nw-webhook-relay", "nw-geo-redirect"}
	for i, w := range workers {
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: cfAccountProd,
			Type: "cloudflare.worker_script", ID: w, Name: w, Status: "deployed",
			CreatedAt: created(250 + i*2),
			Tags:      tags("usage_model", "bundled", "handlers", "fetch", "placement_mode", "smart"),
			Raw:       raw(map[string]any{"id": w, "usage_model": "bundled"}),
		})
	}

	pages := []string{"northwind-marketing", "northwind-docs", "nwlabs-status"}
	for i, p := range pages {
		acct := cfAccountProd
		if i == 2 {
			acct = cfAccountLabs
		}
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: acct,
			Type: "cloudflare.pages_project", ID: uid("cf", "pages", p), Name: p, Status: "success",
			CreatedAt: created(260 + i*4),
			Tags:      tags("subdomain", p+".pages.dev", "production_branch", "main", "build_command", "npm run build"),
			Raw:       raw(map[string]any{"name": p, "subdomain": p + ".pages.dev"}),
		})
	}

	kvs := []string{"nw-sessions", "nw-feature-flags", "nw-geo-cache"}
	for i, k := range kvs {
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: cfAccountProd,
			Type: "cloudflare.kv_namespace", ID: uid("cf", "kv", k), Name: k, Status: "active",
			CreatedAt: created(270 + i),
			Tags:      tags("supports_url_encoding", "true"),
			Raw:       raw(map[string]any{"title": k}),
		})
	}

	d1s := []string{"nw-orders-edge", "nw-lab-metrics"}
	for i, d := range d1s {
		acct := cfAccountProd
		if i == 1 {
			acct = cfAccountLabs
		}
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: acct,
			Type: "cloudflare.d1_database", ID: uid("cf", "d1", d), Name: d, Status: "active",
			CreatedAt: created(275 + i),
			Tags:      tags("version", "production", "num_tables", itoa(6+i*3)),
			Raw:       raw(map[string]any{"name": d, "version": "production"}),
		})
	}

	lbs := []struct{ name, zone string }{
		{"northwind.example lb", cfZoneExample},
		{"northwind.io lb", cfZoneIO},
	}
	for i, lb := range lbs {
		out = append(out, core.Asset{
			Provider: "cloudflare", AccountID: cfAccountProd,
			Type: "cloudflare.load_balancer", ID: uid("cf", "lb", lb.name), Name: lb.name, Status: "enabled",
			CreatedAt: created(280 + i),
			Tags:      tags("zone_id", lb.zone, "steering_policy", "geo", "proxied", "true", "pools", "2"),
			Raw:       raw(map[string]any{"name": lb.name, "steering_policy": "geo"}),
		})
	}

	return out
}

// ----------------------------------------------------------------------
// OCI
// ----------------------------------------------------------------------

type ociVCN struct {
	id, name, region, compartment, cidr string
}

var ociVCNs = []ociVCN{
	{"ocid1.vcn.oc1.eu-frankfurt-1.aaaaaaaanwprodvcn01", "nw-prod-vcn", "eu-frankfurt-1", ociCompartmentProd, "10.20.0.0/16"},
	{"ocid1.vcn.oc1.eu-frankfurt-1.aaaaaaaanwstagevcn1", "nw-stage-vcn", "eu-frankfurt-1", ociCompartmentStg, "10.30.0.0/16"},
	{"ocid1.vcn.oc1.uk-london-1.aaaaaaaanwplatvcn0001", "nw-plat-vcn", "uk-london-1", ociCompartmentPlat, "10.40.0.0/16"},
}

func ociAssets() []core.Asset {
	out := make([]core.Asset, 0, 140)
	out = append(out, ociCompartments()...)
	out = append(out, ociNetwork()...)
	out = append(out, ociCompute()...)
	out = append(out, ociData()...)
	out = append(out, ociIAM()...)
	return out
}

func ociAsset(typ, id, name, region, status string, hourOffset int, tg map[string]string, rawPayload any) core.Asset {
	return core.Asset{
		Provider:  "oci",
		AccountID: ociTenancy,
		Region:    region,
		Type:      typ,
		ID:        id,
		Name:      name,
		Status:    status,
		CreatedAt: created(hourOffset),
		Tags:      tg,
		Raw:       raw(rawPayload),
	}
}

func ociCompartments() []core.Asset {
	comps := []struct{ id, name, parent, desc string }{
		{ociTenancy, "northwind (root)", "", "Tenancy root compartment"},
		{ociCompartmentPlat, "northwind-platform", ociTenancy, "Shared platform services"},
		{ociCompartmentProd, "northwind-prod", ociCompartmentPlat, "Production workloads"},
		{ociCompartmentStg, "northwind-staging", ociCompartmentPlat, "Pre-production workloads"},
	}
	out := make([]core.Asset, 0, len(comps))
	for i, c := range comps {
		out = append(out, ociAsset("oci.compartment", c.id, c.name, "", "ACTIVE", 300+i,
			tags("compartment_id", c.parent, "description", c.desc, "is_accessible", "true"),
			map[string]any{"id": c.id, "name": c.name, "compartmentId": c.parent}))
	}
	return out
}

// ociNetwork builds the VCN backbone. Every subnet and gateway carries the
// vcn_id (and NLBs the subnet_id) that ociNetworkContainment joins on — this
// section is the whole reason network-containment edges exist in the demo.
func ociNetwork() []core.Asset {
	out := make([]core.Asset, 0, 40)
	subnetIDs := make([]string, 0, 9)

	for vi, v := range ociVCNs {
		out = append(out, ociAsset("oci.vcn", v.id, v.name, v.region, "AVAILABLE", 310+vi,
			tags("compartment_id", v.compartment, "cidr_blocks", v.cidr, "dns_label", strings.ReplaceAll(v.name, "-", "")),
			map[string]any{"id": v.id, "displayName": v.name, "cidrBlocks": []string{v.cidr}}))

		for si, tier := range []string{"public", "app", "db"} {
			id := fmt.Sprintf("ocid1.subnet.oc1.%s.aaaaaaaa%s%s", v.region, strings.ReplaceAll(v.name, "-", ""), tier)
			subnetIDs = append(subnetIDs, id)
			out = append(out, ociAsset("oci.subnet", id, v.name+"-"+tier, v.region, "AVAILABLE", 312+vi*3+si,
				tags(
					"compartment_id", v.compartment,
					"vcn_id", v.id,
					"cidr_block", fmt.Sprintf("10.%d.%d.0/24", 20+vi*10, si*16),
					"prohibit_public_ip_on_vnic", strconv.FormatBool(tier != "public"),
				),
				map[string]any{"id": id, "vcnId": v.id, "displayName": v.name + "-" + tier}))
		}

		gateways := []struct{ typ, suffix, extraKey, extraVal string }{
			{"oci.internet_gateway", "igw", "enabled", "true"},
			{"oci.nat_gateway", "nat", "nat_ip", fmt.Sprintf("203.0.113.%d", 40+vi)},
			{"oci.service_gateway", "svcgw", "services", "all-fra-services-in-oracle-services-network"},
		}
		for gi, g := range gateways {
			id := fmt.Sprintf("ocid1.%s.oc1.%s.aaaaaaaa%s%s", strings.TrimPrefix(g.typ, "oci."), v.region, strings.ReplaceAll(v.name, "-", ""), g.suffix)
			out = append(out, ociAsset(g.typ, id, v.name+"-"+g.suffix, v.region, "AVAILABLE", 320+vi*3+gi,
				tags("compartment_id", v.compartment, "vcn_id", v.id, g.extraKey, g.extraVal),
				map[string]any{"id": id, "vcnId": v.id}))
		}
	}

	// Local peering between prod and staging.
	for i, v := range ociVCNs[:2] {
		id := fmt.Sprintf("ocid1.localpeeringgateway.oc1.%s.aaaaaaaanwlpg%d", v.region, i)
		out = append(out, ociAsset("oci.local_peering_gateway", id, v.name+"-lpg", v.region, "AVAILABLE", 330+i,
			tags("compartment_id", v.compartment, "vcn_id", v.id, "peering_status", "PEERED"),
			map[string]any{"id": id, "vcnId": v.id, "peeringStatus": "PEERED"}))
	}

	out = append(out, ociAsset("oci.drg", "ocid1.drg.oc1.eu-frankfurt-1.aaaaaaaanwdrg0001", "nw-corp-drg", "eu-frankfurt-1", "AVAILABLE", 335,
		tags("compartment_id", ociCompartmentPlat, "attachments", "2"),
		map[string]any{"id": "ocid1.drg.oc1.eu-frankfurt-1.aaaaaaaanwdrg0001"}))

	// Load balancers. The prod LB's public IP is also the K8s edge Service's
	// external IP — that pairing is what produces the lb-backend edge.
	lbs := []struct {
		id, name, region, compartment, ips, shape string
	}{
		{"ocid1.loadbalancer.oc1.eu-frankfurt-1.aaaaaaaanwprodlb01", "nw-prod-lb", "eu-frankfurt-1", ociCompartmentProd, ociProdLBIP, "flexible"},
		{"ocid1.loadbalancer.oc1.eu-frankfurt-1.aaaaaaaanwstagelb1", "nw-stage-lb", "eu-frankfurt-1", ociCompartmentStg, ociStageLBIP, "flexible"},
		{"ocid1.loadbalancer.oc1.uk-london-1.aaaaaaaanwplatlb0001", "nw-plat-lb", "uk-london-1", ociCompartmentPlat, ociPlatLBIP, "100Mbps"},
	}
	for i, lb := range lbs {
		out = append(out, ociAsset("oci.load_balancer", lb.id, lb.name, lb.region, "ACTIVE", 340+i,
			tags("compartment_id", lb.compartment, "shape", lb.shape, "ip_addresses", lb.ips, "is_private", "false"),
			map[string]any{"id": lb.id, "displayName": lb.name, "ipAddresses": []any{map[string]any{"ipAddress": lb.ips, "isPublic": true}}}))
	}

	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("ocid1.networkloadbalancer.oc1.eu-frankfurt-1.aaaaaaaanwnlb%d", i)
		out = append(out, ociAsset("oci.network_load_balancer", id, fmt.Sprintf("nw-nlb-%02d", i), "eu-frankfurt-1", "ACTIVE", 344+i,
			tags("compartment_id", ociCompartmentProd, "subnet_id", subnetIDs[i*3], "is_private", "true"),
			map[string]any{"id": id, "subnetId": subnetIDs[i*3]}))
	}

	// OKE control planes, anchored to the VCN that hosts them — this is the
	// edge that ties the Kubernetes subgraph into the OCI network.
	okes := []struct{ id, name, vcn, region, version string }{
		{"ocid1.cluster.oc1.eu-frankfurt-1.aaaaaaaanwprodoke1", "nw-prod-oke", ociVCNs[0].id, "eu-frankfurt-1", "v1.30.1"},
		{"ocid1.cluster.oc1.eu-frankfurt-1.aaaaaaaanwstageoke", "nw-stage-oke", ociVCNs[1].id, "eu-frankfurt-1", "v1.29.4"},
	}
	for i, k := range okes {
		out = append(out, ociAsset("oci.oke.cluster", k.id, k.name, k.region, "ACTIVE", 348+i,
			tags("compartment_id", ociVCNs[i].compartment, "vcn_id", k.vcn, "kubernetes_version", k.version, "node_pools", itoa(2+i)),
			map[string]any{"id": k.id, "name": k.name, "vcnId": k.vcn, "kubernetesVersion": k.version}))
	}

	return out
}

func ociCompute() []core.Asset {
	out := make([]core.Asset, 0, 60)
	r := newRNG(0x5eed03)
	shapes := []string{"VM.Standard.E4.Flex", "VM.Standard.A1.Flex", "VM.Standard3.Flex", "BM.Standard.E4.128"}
	roles := []string{"app", "worker", "cache", "build", "bastion", "gateway"}

	for i := 0; i < 24; i++ {
		v := ociVCNs[i%len(ociVCNs)]
		name := fmt.Sprintf("nw-%s-%s", r.pick(roles), pad(i))
		id := fmt.Sprintf("ocid1.instance.oc1.%s.aaaaaaaanwinst%s", v.region, pad(i))
		status := "RUNNING"
		if i%11 == 7 {
			status = "STOPPED"
		}
		out = append(out, ociAsset("oci.compute.instance", id, name, v.region, status, 360+i,
			tags(
				"compartment_id", v.compartment,
				"shape", r.pick(shapes),
				"availability_domain", fmt.Sprintf("fzTb:%s-AD-%d", strings.ToUpper(v.region), i%3+1),
				"fault_domain", fmt.Sprintf("FAULT-DOMAIN-%d", i%3+1),
				"ocpus", itoa(2+i%6),
				"memory_gb", itoa(16+(i%6)*16),
				"env", map[bool]string{true: "prod", false: "staging"}[v.compartment == ociCompartmentProd],
			),
			map[string]any{"id": id, "displayName": name, "lifecycleState": status}))
	}

	for i := 0; i < 18; i++ {
		v := ociVCNs[i%len(ociVCNs)]
		id := fmt.Sprintf("ocid1.volume.oc1.%s.aaaaaaaanwvol%s", v.region, pad(i))
		out = append(out, ociAsset("oci.block_volume", id, fmt.Sprintf("nw-data-%s", pad(i)), v.region, "AVAILABLE", 390+i,
			tags("compartment_id", v.compartment, "size_gb", itoa(50+(i%8)*50), "vpus_per_gb", "10"),
			map[string]any{"id": id, "sizeInGBs": 50 + (i%8)*50}))
	}
	for i := 0; i < 12; i++ {
		v := ociVCNs[i%len(ociVCNs)]
		id := fmt.Sprintf("ocid1.bootvolume.oc1.%s.aaaaaaaanwboot%s", v.region, pad(i))
		out = append(out, ociAsset("oci.boot_volume", id, fmt.Sprintf("nw-boot-%s", pad(i)), v.region, "AVAILABLE", 410+i,
			tags("compartment_id", v.compartment, "size_gb", "50", "image_id", "ocid1.image.oc1..aaaaaaaaol8"),
			map[string]any{"id": id, "sizeInGBs": 50}))
	}
	return out
}

func ociData() []core.Asset {
	out := make([]core.Asset, 0, 20)

	buckets := []string{"nw-orders-archive", "nw-invoices", "nw-etl-landing", "nw-etl-curated", "nw-db-backups", "nw-audit-logs", "nw-stage-scratch", "nw-terraform"}
	for i, b := range buckets {
		comp := ociCompartmentProd
		if i >= 6 {
			comp = ociCompartmentStg
		}
		id := "ocid1.bucket.oc1.eu-frankfurt-1.aaaaaaaa" + strings.ReplaceAll(b, "-", "")
		out = append(out, ociAsset("oci.object_storage.bucket", id, b, "eu-frankfurt-1", "ACTIVE", 430+i,
			tags("compartment_id", comp, "namespace", "frnwnorthwind", "storage_tier", "Standard",
				"public_access_type", "NoPublicAccess", "versioning", "Enabled", "approximate_count", itoa(1200+i*431)),
			map[string]any{"name": b, "namespace": "frnwnorthwind", "storageTier": "Standard"}))
	}

	adbs := []struct{ name, comp, workload string }{
		{"nw-orders-adb", ociCompartmentProd, "OLTP"},
		{"nw-analytics-adw", ociCompartmentPlat, "DW"},
	}
	for i, d := range adbs {
		id := fmt.Sprintf("ocid1.autonomousdatabase.oc1.eu-frankfurt-1.aaaaaaaanwadb%d", i)
		out = append(out, ociAsset("oci.autonomous_database", id, d.name, "eu-frankfurt-1", "AVAILABLE", 440+i,
			tags("compartment_id", d.comp, "workload", d.workload, "ocpu_count", itoa(2+i*2),
				"storage_tb", itoa(1+i), "license_model", "LICENSE_INCLUDED", "is_mtls_required", "true"),
			map[string]any{"id": id, "dbName": d.name, "dbWorkload": d.workload}))
	}

	out = append(out, ociAsset("oci.db_system", "ocid1.dbsystem.oc1.uk-london-1.aaaaaaaanwmysql01", "nw-legacy-mysql", "uk-london-1", "AVAILABLE", 443,
		tags("compartment_id", ociCompartmentPlat, "shape", "MySQL.VM.Standard.E4.1.8GB", "database_edition", "ENTERPRISE_EDITION"),
		map[string]any{"id": "ocid1.dbsystem.oc1.uk-london-1.aaaaaaaanwmysql01"}))

	for i, name := range []string{"nw-prod-vault", "nw-platform-vault"} {
		comp := []string{ociCompartmentProd, ociCompartmentPlat}[i]
		id := fmt.Sprintf("ocid1.vault.oc1.eu-frankfurt-1.aaaaaaaanwvault%d", i)
		out = append(out, ociAsset("oci.vault", id, name, "eu-frankfurt-1", "ACTIVE", 445+i,
			tags("compartment_id", comp, "vault_type", "DEFAULT", "keys", itoa(4+i*3)),
			map[string]any{"id": id, "displayName": name, "vaultType": "DEFAULT"}))
	}

	for i, name := range []string{"nw-order-events", "nw-image-thumbnailer", "nw-invoice-pdf"} {
		id := fmt.Sprintf("ocid1.fnfunc.oc1.eu-frankfurt-1.aaaaaaaanwfn%d", i)
		out = append(out, ociAsset("oci.functions.function", id, name, "eu-frankfurt-1", "ACTIVE", 448+i,
			tags("compartment_id", ociCompartmentProd, "memory_mb", "256", "timeout_seconds", "60"),
			map[string]any{"id": id, "displayName": name}))
	}
	out = append(out, ociAsset("oci.functions.application", "ocid1.fnapp.oc1.eu-frankfurt-1.aaaaaaaanwfnapp1", "nw-serverless", "eu-frankfurt-1", "ACTIVE", 452,
		tags("compartment_id", ociCompartmentProd, "subnet_ids", "1"),
		map[string]any{"id": "ocid1.fnapp.oc1.eu-frankfurt-1.aaaaaaaanwfnapp1"}))

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("ocid1.computecontainerinstance.oc1.eu-frankfurt-1.aaaaaaaanwci%d", i)
		out = append(out, ociAsset("oci.container_instance", id, fmt.Sprintf("nw-batch-%02d", i), "eu-frankfurt-1", "ACTIVE", 454+i,
			tags("compartment_id", ociCompartmentStg, "shape", "CI.Standard.E4.Flex", "containers", "1"),
			map[string]any{"id": id}))
	}

	return out
}

func ociIAM() []core.Asset {
	out := make([]core.Asset, 0, 30)
	r := newRNG(0x5eed04)
	first := []string{"amara", "bo", "chen", "dara", "elif", "farid", "gita", "hugo", "iris", "jonas", "kaya", "luca"}

	for i, f := range first {
		id := fmt.Sprintf("ocid1.user.oc1..aaaaaaaanwuser%s", pad(i))
		status := "ACTIVE"
		if i%7 == 5 {
			status = "INACTIVE"
		}
		out = append(out, ociAsset("oci.iam.user", id, f+"@northwind.example", "", status, 460+i,
			tags("compartment_id", ociTenancy, "email_verified", strconv.FormatBool(i%4 != 3),
				"is_mfa_activated", strconv.FormatBool(i%3 != 0), "capabilities_api_keys", "true"),
			map[string]any{"id": id, "name": f + "@northwind.example"}))
	}

	groups := []string{"Administrators", "PlatformEngineers", "ReadOnlyAuditors", "Developers", "BillingAdmins"}
	for i, g := range groups {
		id := fmt.Sprintf("ocid1.group.oc1..aaaaaaaanwgroup%s", pad(i))
		out = append(out, ociAsset("oci.iam.group", id, g, "", "ACTIVE", 475+i,
			tags("compartment_id", ociTenancy, "members", itoa(1+r.n(8))),
			map[string]any{"id": id, "name": g}))
	}

	policies := []struct{ name, comp string }{
		{"platform-admins", ociCompartmentPlat},
		{"prod-readonly", ociCompartmentProd},
		{"oke-node-policy", ociCompartmentProd},
		{"staging-developers", ociCompartmentStg},
		{"object-storage-etl", ociCompartmentPlat},
		{"tenancy-auditors", ociTenancy},
	}
	for i, p := range policies {
		id := fmt.Sprintf("ocid1.policy.oc1..aaaaaaaanwpolicy%s", pad(i))
		out = append(out, ociAsset("oci.iam.policy", id, p.name, "", "ACTIVE", 482+i,
			tags("compartment_id", p.comp, "statements", itoa(2+i%4)),
			map[string]any{"id": id, "name": p.name, "statements": []string{"Allow group PlatformEngineers to manage all-resources in compartment northwind-platform"}}))
	}

	for i, d := range []string{"oke-nodes", "fn-invokers", "instance-agents"} {
		id := fmt.Sprintf("ocid1.dynamicgroup.oc1..aaaaaaaanwdg%s", pad(i))
		out = append(out, ociAsset("oci.iam.dynamic_group", id, d, "", "ACTIVE", 490+i,
			tags("compartment_id", ociTenancy, "matching_rule", "ALL {instance.compartment.id = '"+ociCompartmentProd+"'}"),
			map[string]any{"id": id, "name": d}))
	}

	return out
}

// ----------------------------------------------------------------------
// Kubernetes
// ----------------------------------------------------------------------

type kubeWorkload struct {
	cluster, ns, app, kind string
	replicas               int
}

var kubeWorkloads = []kubeWorkload{
	{kubeProdCluster, "nw-edge", "edge-gateway", "Deployment", 3},
	{kubeProdCluster, "nw-edge", "cert-rotator", "Deployment", 1},
	{kubeProdCluster, "nw-shop", "storefront", "Deployment", 4},
	{kubeProdCluster, "nw-shop", "catalog", "Deployment", 3},
	{kubeProdCluster, "nw-shop", "search", "Deployment", 2},
	{kubeProdCluster, "nw-payments", "payments-api", "Deployment", 3},
	{kubeProdCluster, "nw-payments", "ledger", "StatefulSet", 3},
	{kubeProdCluster, "nw-platform", "redis", "StatefulSet", 3},
	{kubeProdCluster, "nw-platform", "log-shipper", "DaemonSet", 4},
	{kubeProdCluster, "monitoring", "prometheus", "StatefulSet", 2},
	{kubeProdCluster, "monitoring", "grafana", "Deployment", 1},
	{kubeStageCluster, "nw-edge", "stage-gateway", "Deployment", 2},
	{kubeStageCluster, "nw-shop", "storefront", "Deployment", 2},
	{kubeStageCluster, "nw-shop", "catalog", "Deployment", 2},
	{kubeStageCluster, "sandbox", "scratch", "Deployment", 1},
	{kubeStageCluster, "monitoring", "prometheus", "StatefulSet", 1},
}

type kubeService struct {
	cluster, ns, name string
	selector          map[string]string
	svcType           string
	lbIP, lbHost      string
	port              int
}

// kubeServices: edge-gateway republishes the OCI prod LB's IP (→ lb-backend),
// stage-gateway publishes a hostname a Cloudflare CNAME targets (→ dns), and
// every selector-bearing Service matches pods above (→ service-backend).
var kubeServices = []kubeService{
	{kubeProdCluster, "nw-edge", "edge-gateway", map[string]string{"app": "edge-gateway"}, "LoadBalancer", ociProdLBIP, "", 443},
	{kubeProdCluster, "nw-shop", "storefront", map[string]string{"app": "storefront"}, "ClusterIP", "", "", 80},
	{kubeProdCluster, "nw-shop", "catalog", map[string]string{"app": "catalog"}, "ClusterIP", "", "", 8080},
	{kubeProdCluster, "nw-shop", "search", map[string]string{"app": "search"}, "ClusterIP", "", "", 9200},
	{kubeProdCluster, "nw-payments", "payments-api", map[string]string{"app": "payments-api"}, "ClusterIP", "", "", 8443},
	{kubeProdCluster, "nw-payments", "ledger", map[string]string{"app": "ledger"}, "ClusterIP", "", "", 5432},
	{kubeProdCluster, "nw-platform", "redis", map[string]string{"app": "redis"}, "ClusterIP", "", "", 6379},
	{kubeProdCluster, "monitoring", "prometheus", map[string]string{"app": "prometheus"}, "ClusterIP", "", "", 9090},
	{kubeProdCluster, "monitoring", "grafana", map[string]string{"app": "grafana"}, "ClusterIP", "", "", 3000},
	{kubeStageCluster, "nw-edge", "stage-gateway", map[string]string{"app": "stage-gateway"}, "LoadBalancer", "", kubeStageLBHost, 443},
	{kubeStageCluster, "nw-shop", "storefront", map[string]string{"app": "storefront"}, "ClusterIP", "", "", 80},
	{kubeStageCluster, "nw-shop", "catalog", map[string]string{"app": "catalog"}, "ClusterIP", "", "", 8080},
	// ExternalName services carry no selector; the resolver must not treat
	// that as "select everything".
	{kubeStageCluster, "sandbox", "legacy-erp", nil, "ExternalName", "", "", 0},
}

func kubernetesAssets() []core.Asset {
	out := make([]core.Asset, 0, 150)
	out = append(out, kubeNamespaces()...)
	out = append(out, kubeWorkloadAssets()...)
	out = append(out, kubeServiceAssets()...)
	out = append(out, kubeGatewayAssets()...)
	out = append(out, kubeNetworkPolicies()...)
	out = append(out, kubeSupportingAssets()...)
	return out
}

func kubeAsset(cluster, typ, ns, name, status string, hourOffset int, labels map[string]string, rawPayload any) core.Asset {
	tg := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		tg[k] = v
	}
	if ns != "" {
		tg["namespace"] = ns
	}
	if len(tg) == 0 {
		tg = nil
	}
	return core.Asset{
		Provider:  "kubernetes",
		AccountID: cluster,
		Type:      typ,
		ID:        uid(cluster, typ, ns, name),
		Name:      name,
		Status:    status,
		CreatedAt: created(hourOffset),
		Tags:      tg,
		Raw:       raw(rawPayload),
	}
}

func kubeNamespaces() []core.Asset {
	nss := []struct{ cluster, name string }{
		{kubeProdCluster, "nw-edge"},
		{kubeProdCluster, "nw-shop"},
		{kubeProdCluster, "nw-payments"},
		{kubeProdCluster, "nw-platform"},
		{kubeProdCluster, "monitoring"},
		{kubeStageCluster, "nw-edge"},
		{kubeStageCluster, "nw-shop"},
		{kubeStageCluster, "sandbox"},
		{kubeStageCluster, "monitoring"},
	}
	out := make([]core.Asset, 0, len(nss))
	for i, ns := range nss {
		env := "prod"
		if ns.cluster == kubeStageCluster {
			env = "staging"
		}
		out = append(out, kubeAsset(ns.cluster, "v1.Namespace", "", ns.name, "Active", 500+i,
			map[string]string{"kubernetes.io/metadata.name": ns.name, "env": env},
			map[string]any{
				"apiVersion": "v1", "kind": "Namespace",
				"metadata": map[string]any{"name": ns.name, "labels": map[string]any{"env": env}},
				"status":   map[string]any{"phase": "Active"},
			}))
	}
	return out
}

func kubeWorkloadAssets() []core.Asset {
	out := make([]core.Asset, 0, 60)
	r := newRNG(0x5eed05)
	suffixes := []string{"2k9dl", "7mzqp", "x4v8t", "b6rnc", "q1wsy", "z3hkf", "n8tpv", "j5gbm"}

	for wi, w := range kubeWorkloads {
		apiVersion := "apps/v1"
		labels := map[string]string{"app": w.app, "app.kubernetes.io/name": w.app}
		out = append(out, kubeAsset(w.cluster, apiVersion+"."+w.kind, w.ns, w.app, "Available=True", 520+wi,
			labels,
			map[string]any{
				"apiVersion": apiVersion, "kind": w.kind,
				"metadata": map[string]any{"name": w.app, "namespace": w.ns, "labels": labels},
				"spec": map[string]any{
					"replicas": w.replicas,
					"selector": map[string]any{"matchLabels": map[string]any{"app": w.app}},
				},
				"status": map[string]any{"readyReplicas": w.replicas, "replicas": w.replicas},
			}))

		// A pod-template-hash shaped like the real thing, derived from the
		// workload identity so it never changes between runs.
		h := fnv.New32a()
		_, _ = h.Write([]byte(w.cluster + "/" + w.ns + "/" + w.app))
		hash := fmt.Sprintf("%x", h.Sum32()&0xffffff)
		// Offsetting the random pick by the replica index keeps the suffixes
		// distinct within a workload — two pods of one Deployment sharing a
		// name would collide on the content-derived UID.
		base := r.n(len(suffixes))
		for pi := 0; pi < w.replicas; pi++ {
			podName := fmt.Sprintf("%s-%s-%s", w.app, hash, suffixes[(base+pi)%len(suffixes)])
			phase := "Running"
			if wi%9 == 4 && pi == 0 {
				phase = "Pending"
			}
			podLabels := map[string]string{
				"app":                    w.app,
				"app.kubernetes.io/name": w.app,
				"pod-template-hash":      hash,
			}
			out = append(out, kubeAsset(w.cluster, "v1.Pod", w.ns, podName, phase, 540+wi*4+pi,
				podLabels,
				map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": podName, "namespace": w.ns, "labels": podLabels},
					"spec": map[string]any{
						"nodeName":   fmt.Sprintf("oke-node-%02d", pi+wi%4),
						"containers": []any{map[string]any{"name": w.app, "image": "registry.northwind.example/" + w.app + ":1." + itoa(wi) + "." + itoa(pi)}},
					},
					"status": map[string]any{"phase": phase, "podIP": fmt.Sprintf("10.244.%d.%d", wi, pi+2)},
				}))
		}
	}
	return out
}

func kubeServiceAssets() []core.Asset {
	out := make([]core.Asset, 0, len(kubeServices))
	for i, s := range kubeServices {
		spec := map[string]any{"type": s.svcType}
		if len(s.selector) > 0 {
			sel := make(map[string]any, len(s.selector))
			for k, v := range s.selector {
				sel[k] = v
			}
			spec["selector"] = sel
		}
		if s.port > 0 {
			spec["ports"] = []any{map[string]any{"name": "http", "port": s.port, "targetPort": s.port, "protocol": "TCP"}}
		}
		if s.svcType == "ExternalName" {
			spec["externalName"] = "erp.northwind.example"
		}

		status := map[string]any{}
		if s.lbIP != "" {
			status["loadBalancer"] = map[string]any{"ingress": []any{map[string]any{"ip": s.lbIP}}}
		}
		if s.lbHost != "" {
			status["loadBalancer"] = map[string]any{"ingress": []any{map[string]any{"hostname": s.lbHost}}}
		}

		out = append(out, kubeAsset(s.cluster, "v1.Service", s.ns, s.name, "", 600+i,
			map[string]string{"app": s.name},
			map[string]any{
				"apiVersion": "v1", "kind": "Service",
				"metadata": map[string]any{"name": s.name, "namespace": s.ns},
				"spec":     spec,
				"status":   status,
			}))
	}
	return out
}

// kubeGatewayAssets are the Ingresses and HTTPRoutes whose Raw specs name the
// Services above — the gateway-route edges.
func kubeGatewayAssets() []core.Asset {
	type backend struct {
		path, svc string
		port      int
	}
	ingresses := []struct {
		cluster, ns, name, host string
		backends                []backend
	}{
		{kubeProdCluster, "nw-shop", "shop-ingress", "shop.northwind.example", []backend{{"/", "storefront", 80}, {"/catalog", "catalog", 8080}}},
		{kubeProdCluster, "nw-edge", "edge-ingress", "www.northwind.example", []backend{{"/", "edge-gateway", 443}}},
		{kubeProdCluster, "monitoring", "grafana-ingress", "grafana.northwind.example", []backend{{"/", "grafana", 3000}}},
		{kubeStageCluster, "nw-shop", "stage-shop-ingress", "stage.northwind.io", []backend{{"/", "storefront", 80}}},
	}

	out := make([]core.Asset, 0, 8)
	for i, ing := range ingresses {
		paths := make([]any, 0, len(ing.backends))
		for _, b := range ing.backends {
			paths = append(paths, map[string]any{
				"path":     b.path,
				"pathType": "Prefix",
				"backend": map[string]any{"service": map[string]any{
					"name": b.svc,
					"port": map[string]any{"number": b.port},
				}},
			})
		}
		out = append(out, kubeAsset(ing.cluster, "networking.k8s.io/v1.Ingress", ing.ns, ing.name, "", 620+i,
			map[string]string{"app.kubernetes.io/managed-by": "helm"},
			map[string]any{
				"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
				"metadata": map[string]any{"name": ing.name, "namespace": ing.ns},
				"spec": map[string]any{
					"ingressClassName": "nginx",
					"rules":            []any{map[string]any{"host": ing.host, "http": map[string]any{"paths": paths}}},
				},
			}))
	}

	routes := []struct {
		cluster, ns, name, host, svc string
		port                         int
	}{
		{kubeProdCluster, "nw-payments", "payments-route", "pay.northwind.example", "payments-api", 8443},
		{kubeStageCluster, "nw-edge", "stage-route", "stage.northwind.io", "stage-gateway", 443},
	}
	for i, rt := range routes {
		out = append(out, kubeAsset(rt.cluster, "gateway.networking.k8s.io/v1.HTTPRoute", rt.ns, rt.name, "", 626+i,
			map[string]string{"app.kubernetes.io/managed-by": "helm"},
			map[string]any{
				"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
				"metadata": map[string]any{"name": rt.name, "namespace": rt.ns},
				"spec": map[string]any{
					"hostnames":  []any{rt.host},
					"parentRefs": []any{map[string]any{"name": "nw-gateway"}},
					"rules": []any{map[string]any{
						"backendRefs": []any{map[string]any{"name": rt.svc, "port": rt.port}},
					}},
				},
			}))
	}
	return out
}

// kubeNetworkPolicies drive the traffic-allow edges. Note the deliberate
// default-deny with an EMPTY podSelector: it selects every pod in its
// namespace and, having no ingress rules, authorises nothing — the shape the
// resolver has to handle differently from a Service's empty selector.
func kubeNetworkPolicies() []core.Asset {
	policies := []struct {
		cluster, ns, name string
		spec              map[string]any
	}{
		{kubeProdCluster, "nw-shop", "storefront-allow-catalog", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "storefront"}},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{
				"from":  []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "catalog"}}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 8080}},
			}},
		}},
		{kubeProdCluster, "nw-payments", "payments-allow-storefront", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "payments-api"}},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{
				"from": []any{map[string]any{
					"namespaceSelector": map[string]any{},
					"podSelector":       map[string]any{"matchLabels": map[string]any{"app": "storefront"}},
				}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 8443}},
			}},
		}},
		{kubeProdCluster, "nw-payments", "ledger-egress-redis", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "ledger"}},
			"policyTypes": []any{"Egress"},
			"egress": []any{map[string]any{
				"to": []any{map[string]any{
					"namespaceSelector": map[string]any{},
					"podSelector":       map[string]any{"matchLabels": map[string]any{"app": "redis"}},
				}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 6379}},
			}},
		}},
		{kubeProdCluster, "nw-platform", "default-deny-ingress", map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress"},
		}},
		{kubeStageCluster, "nw-shop", "storefront-allow-catalog", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "storefront"}},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{
				"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "catalog"}}}},
			}},
		}},
		{kubeStageCluster, "sandbox", "default-deny-all", map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress", "Egress"},
		}},
	}

	out := make([]core.Asset, 0, len(policies))
	for i, p := range policies {
		out = append(out, kubeAsset(p.cluster, "networking.k8s.io/v1.NetworkPolicy", p.ns, p.name, "", 640+i,
			nil,
			map[string]any{
				"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
				"metadata": map[string]any{"name": p.name, "namespace": p.ns},
				"spec":     p.spec,
			}))
	}
	return out
}

// kubeSupportingAssets are the ConfigMaps / Secrets / ServiceAccounts / PVCs
// and one CRD kind — no edges, but they are most of what a real cluster
// inventory is made of, and the demo should look like one.
func kubeSupportingAssets() []core.Asset {
	out := make([]core.Asset, 0, 60)
	r := newRNG(0x5eed06)
	nss := []struct{ cluster, ns string }{
		{kubeProdCluster, "nw-edge"}, {kubeProdCluster, "nw-shop"}, {kubeProdCluster, "nw-payments"},
		{kubeProdCluster, "nw-platform"}, {kubeProdCluster, "monitoring"},
		{kubeStageCluster, "nw-edge"}, {kubeStageCluster, "nw-shop"}, {kubeStageCluster, "sandbox"},
	}
	kinds := []string{"config", "settings", "features", "routes", "limits"}

	for i := 0; i < 20; i++ {
		n := nss[i%len(nss)]
		name := fmt.Sprintf("%s-%s", r.pick(kinds), pad(i))
		out = append(out, kubeAsset(n.cluster, "v1.ConfigMap", n.ns, name, "", 660+i,
			map[string]string{"app.kubernetes.io/managed-by": "helm"},
			map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": name, "namespace": n.ns},
				"data":     map[string]any{"LOG_LEVEL": "info"}}))
	}
	for i := 0; i < 12; i++ {
		n := nss[i%len(nss)]
		name := fmt.Sprintf("tls-%s", pad(i))
		out = append(out, kubeAsset(n.cluster, "v1.Secret", n.ns, name, "", 685+i,
			map[string]string{"app.kubernetes.io/managed-by": "cert-manager"},
			map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
				"metadata": map[string]any{"name": name, "namespace": n.ns}}))
	}
	for i := 0; i < 8; i++ {
		n := nss[i%len(nss)]
		name := fmt.Sprintf("sa-%s", pad(i))
		out = append(out, kubeAsset(n.cluster, "v1.ServiceAccount", n.ns, name, "", 700+i,
			nil,
			map[string]any{"apiVersion": "v1", "kind": "ServiceAccount",
				"metadata": map[string]any{"name": name, "namespace": n.ns}}))
	}
	for i := 0; i < 6; i++ {
		n := nss[i%len(nss)]
		name := fmt.Sprintf("data-%s", pad(i))
		out = append(out, kubeAsset(n.cluster, "v1.PersistentVolumeClaim", n.ns, name, "Bound", 710+i,
			nil,
			map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": map[string]any{"name": name, "namespace": n.ns},
				"spec":     map[string]any{"storageClassName": "oci-bv", "resources": map[string]any{"requests": map[string]any{"storage": itoa(10+i*10) + "Gi"}}},
				"status":   map[string]any{"phase": "Bound"}}))
	}
	// A CRD kind, to show discovery-driven collection picking up types the
	// tool has never heard of.
	hosts := []string{"northwind.example", "www.northwind.example", "shop.northwind.example", "pay.northwind.example", "grafana.northwind.example", "stage.northwind.io"}
	for i, h := range hosts {
		cluster := kubeProdCluster
		ns := "nw-edge"
		if i == 5 {
			cluster, ns = kubeStageCluster, "nw-edge"
		}
		out = append(out, kubeAsset(cluster, "cert-manager.io/v1.Certificate", ns, h, "Ready=True", 720+i,
			nil,
			map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
				"metadata": map[string]any{"name": h, "namespace": ns},
				"spec":     map[string]any{"dnsNames": []any{h}, "issuerRef": map[string]any{"name": "letsencrypt-prod"}}}))
	}
	return out
}

// ----------------------------------------------------------------------
// GCP
// ----------------------------------------------------------------------

// gcpAssets mirrors what Cloud Asset Inventory returns: full resource names as
// ids, assetType as the type. The forwarding rule publishes the address two
// Cloudflare A records point at — there is no GCP address resolver yet, so
// that pair is currently a documented gap rather than an edge.
func gcpAssets() []core.Asset {
	out := make([]core.Asset, 0, 64)
	r := newRNG(0x5eed07)
	zones := []string{"europe-west3-a", "europe-west3-b", "europe-west4-a", "us-central1-c"}
	machines := []string{"e2-standard-4", "n2-standard-8", "c3-highmem-4", "e2-medium"}

	add := func(typ, name, display, location, state string, hourOffset int, tg map[string]string, rawPayload any) {
		out = append(out, core.Asset{
			Provider:  "gcp",
			AccountID: gcpProject,
			Region:    location,
			Type:      typ,
			ID:        name,
			Name:      display,
			Status:    state,
			CreatedAt: created(hourOffset),
			Tags:      tg,
			Raw:       raw(rawPayload),
		})
	}

	for i := 0; i < 18; i++ {
		zone := zones[i%len(zones)]
		name := fmt.Sprintf("nw-analytics-%s", pad(i))
		full := fmt.Sprintf("//compute.googleapis.com/projects/%s/zones/%s/instances/%s", gcpProject, zone, name)
		state := "RUNNING"
		if i%8 == 6 {
			state = "TERMINATED"
		}
		add("compute.googleapis.com/Instance", full, name, zone, state, 760+i,
			tags("machine_type", r.pick(machines), "env", "prod", "team", "data", "network_tags", "allow-health-checks"),
			map[string]any{"name": full, "assetType": "compute.googleapis.com/Instance", "state": state})
	}

	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("nw-lake-%s", pad(i))
		full := fmt.Sprintf("//storage.googleapis.com/%s", name)
		add("storage.googleapis.com/Bucket", full, name, "europe-west3", "", 790+i,
			tags("storage_class", r.pick([]string{"STANDARD", "NEARLINE", "COLDLINE"}), "team", "data", "uniform_access", "true"),
			map[string]any{"name": full, "assetType": "storage.googleapis.com/Bucket"})
	}

	for i, name := range []string{"nw-analytics-gke", "nw-ml-gke"} {
		full := fmt.Sprintf("//container.googleapis.com/projects/%s/locations/europe-west3/clusters/%s", gcpProject, name)
		add("container.googleapis.com/Cluster", full, name, "europe-west3", "RUNNING", 805+i,
			tags("release_channel", "REGULAR", "node_count", itoa(3+i*2), "team", "data"),
			map[string]any{"name": full, "assetType": "container.googleapis.com/Cluster"})
	}

	fwdRules := []struct{ name, ip string }{
		{"nw-shop-https", gcpFwdRuleIP},
		{"nw-internal-grpc", "10.128.0.44"},
	}
	for i, f := range fwdRules {
		full := fmt.Sprintf("//compute.googleapis.com/projects/%s/global/forwardingRules/%s", gcpProject, f.name)
		add("compute.googleapis.com/ForwardingRule", full, f.name, "global", "", 810+i,
			// ip_addresses (plural), matching what the real provider's
			// buildTags emits from additionalAttributes — the topology index
			// keys off that exact tag, so a fixture using a different spelling
			// would silently model an edge the live provider does produce.
			tags("ip_addresses", f.ip, "load_balancing_scheme", map[bool]string{true: "EXTERNAL_MANAGED", false: "INTERNAL"}[i == 0], "port_range", "443"),
			map[string]any{"name": full, "assetType": "compute.googleapis.com/ForwardingRule", "additionalAttributes": map[string]any{"ipAddress": f.ip}})
	}

	for i, name := range []string{"nw-vpc", "nw-mgmt-vpc"} {
		full := fmt.Sprintf("//compute.googleapis.com/projects/%s/global/networks/%s", gcpProject, name)
		add("compute.googleapis.com/Network", full, name, "global", "", 815+i,
			tags("routing_mode", "REGIONAL", "auto_create_subnetworks", "false"),
			map[string]any{"name": full, "assetType": "compute.googleapis.com/Network"})
	}
	for i := 0; i < 4; i++ {
		region := []string{"europe-west3", "europe-west4", "us-central1", "asia-south1"}[i]
		name := fmt.Sprintf("nw-subnet-%s", region)
		full := fmt.Sprintf("//compute.googleapis.com/projects/%s/regions/%s/subnetworks/%s", gcpProject, region, name)
		add("compute.googleapis.com/Subnetwork", full, name, region, "", 820+i,
			tags("ip_cidr_range", fmt.Sprintf("10.%d.0.0/20", 128+i*8), "private_ip_google_access", "true"),
			map[string]any{"name": full, "assetType": "compute.googleapis.com/Subnetwork"})
	}

	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("nw-svc-%s@%s.iam.gserviceaccount.com", pad(i), gcpProject)
		full := fmt.Sprintf("//iam.googleapis.com/projects/%s/serviceAccounts/%s", gcpProject, name)
		add("iam.googleapis.com/ServiceAccount", full, name, "global", "", 830+i,
			tags("disabled", strconv.FormatBool(i%6 == 5), "team", r.pick([]string{"data", "platform", "shop"})),
			map[string]any{"name": full, "assetType": "iam.googleapis.com/ServiceAccount"})
	}

	for i := 0; i < 12; i++ {
		zone := zones[i%len(zones)]
		name := fmt.Sprintf("nw-disk-%s", pad(i))
		full := fmt.Sprintf("//compute.googleapis.com/projects/%s/zones/%s/disks/%s", gcpProject, zone, name)
		add("compute.googleapis.com/Disk", full, name, zone, "READY", 845+i,
			tags("size_gb", itoa(100+(i%5)*100), "disk_type", "pd-balanced", "team", "data"),
			map[string]any{"name": full, "assetType": "compute.googleapis.com/Disk"})
	}

	for i, name := range []string{"nw-orders-topic", "nw-clickstream-topic"} {
		full := fmt.Sprintf("//pubsub.googleapis.com/projects/%s/topics/%s", gcpProject, name)
		add("pubsub.googleapis.com/Topic", full, name, "global", "", 860+i,
			tags("team", "data", "message_retention", "7d"),
			map[string]any{"name": full, "assetType": "pubsub.googleapis.com/Topic"})
	}

	return out
}

// ----------------------------------------------------------------------
// Tailscale
// ----------------------------------------------------------------------

type tsDevice struct {
	id, name, host, ip, user, os, aclTags string
}

var tsDevices = []tsDevice{
	{"n8fT2cCNTRL", "bastion", "bastion", tsBastionIP, "ops@northwind.example", "linux", "tag:prod,tag:bastion"},
	{"nA1kL9pQRST", "build-runner-01", "build-runner-01", "100.64.0.11", "ci@northwind.example", "linux", "tag:ci"},
	{"nB2mM8rSTUV", "build-runner-02", "build-runner-02", "100.64.0.12", "ci@northwind.example", "linux", "tag:ci"},
	{"nC3nN7sTUVW", "db-jump", "db-jump", "100.64.0.13", "ops@northwind.example", "linux", "tag:prod,tag:db"},
	{"nD4oO6tUVWX", "metrics-relay", "metrics-relay", "100.64.0.14", "ops@northwind.example", "linux", "tag:prod"},
	{"nE5pP5uVWXY", "amara-mbp", "amara-mbp", "100.64.0.21", "amara@northwind.example", "macOS", ""},
	{"nF6qQ4vWXYZ", "chen-thinkpad", "chen-thinkpad", "100.64.0.22", "chen@northwind.example", "linux", ""},
	{"nG7rR3wXYZA", "dara-iphone", "dara-iphone", "100.64.0.23", "dara@northwind.example", "iOS", ""},
	{"nH8sS2xYZAB", "elif-win", "elif-win", "100.64.0.24", "elif@northwind.example", "windows", ""},
	{"nI9tT1yZABC", "farid-mbp", "farid-mbp", "100.64.0.25", "farid@northwind.example", "macOS", ""},
	{"nJ0uU0zABCD", "lab-pi", "lab-pi", "100.64.0.31", "labs@northwind.example", "linux", "tag:lab"},
	{"nK1vV9aBCDE", "lab-nuc", "lab-nuc", "100.64.0.32", "labs@northwind.example", "linux", "tag:lab"},
	{"nL2wW8bCDEF", "subnet-router-fra", "subnet-router-fra", "100.64.0.41", "ops@northwind.example", "linux", "tag:prod,tag:router"},
	{"nM3xX7cDEFG", "subnet-router-lon", "subnet-router-lon", "100.64.0.42", "ops@northwind.example", "linux", "tag:prod,tag:router"},
}

func tailscaleAssets() []core.Asset {
	out := make([]core.Asset, 0, 48)

	for i, d := range tsDevices {
		// The device's MagicDNS name is what a Cloudflare CNAME can target.
		dnsName := d.host + ".tailfe8c.ts.net"
		routes := ""
		if strings.Contains(d.aclTags, "tag:router") {
			routes = fmt.Sprintf("10.%d.0.0/16", 20+i%2*10)
		}
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.device", ID: d.id, Name: d.name, Status: "connected",
			CreatedAt: created(880 + i),
			Tags: tags(
				"ip", d.ip,
				"ipv6", fmt.Sprintf("fd7a:115c:a1e0::%x", 0x10+i),
				"addresses", d.ip,
				"dns_name", dnsName,
				"hostname", d.host,
				"user", d.user,
				"os", d.os,
				"client_version", "1.72.0",
				"authorized", "true",
				"key_expiry_disabled", strconv.FormatBool(d.aclTags != ""),
				"acl_tags", d.aclTags,
				"advertised_routes", routes,
				"enabled_routes", routes,
			),
			Raw: raw(map[string]any{"nodeId": d.id, "name": dnsName, "hostname": d.host, "addresses": []any{d.ip}, "tags": strings.Split(d.aclTags, ",")}),
		})
	}

	users := []struct{ id, login, display, role string }{
		{"uid-1001", "ops@northwind.example", "Northwind Ops", "owner"},
		{"uid-1002", "amara@northwind.example", "Amara Diallo", "admin"},
		{"uid-1003", "chen@northwind.example", "Chen Wei", "member"},
		{"uid-1004", "dara@northwind.example", "Dara Novak", "member"},
		{"uid-1005", "elif@northwind.example", "Elif Kaya", "member"},
		{"uid-1006", "ci@northwind.example", "CI Service", "member"},
	}
	for i, u := range users {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.user", ID: u.id, Name: u.display, Status: "active",
			CreatedAt: created(900 + i),
			Tags: tags("login_name", u.login, "display_name", u.display, "role", u.role,
				"user_type", "member", "device_count", itoa(1+i%3), "currently_connected", strconv.FormatBool(i%2 == 0)),
			Raw: raw(map[string]any{"id": u.id, "loginName": u.login, "role": u.role}),
		})
	}

	for i, k := range []string{"ci-runner-key", "oauth-terraform", "server-provisioning", "lab-ephemeral"} {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.key", ID: fmt.Sprintf("k%s%s", pad(i), "CNTRL"), Name: k, Status: "valid",
			CreatedAt: created(910 + i),
			Tags: tags("key_type", "auth", "reusable", strconv.FormatBool(i%2 == 0),
				"ephemeral", strconv.FormatBool(i == 3), "acl_tags", []string{"tag:ci", "tag:prod", "tag:prod", "tag:lab"}[i]),
			Raw: raw(map[string]any{"id": fmt.Sprintf("k%s%s", pad(i), "CNTRL"), "description": k}),
		})
	}

	out = append(out, core.Asset{
		Provider: "tailscale", AccountID: tailnetName,
		Type: "tailscale.dns", ID: "dns:" + tailnetName, Name: "Tailnet DNS", Status: "active",
		CreatedAt: created(915),
		Tags:      tags("magic_dns", "true", "nameservers", "1.1.1.1,9.9.9.9", "search_paths", "northwind.example"),
		Raw:       raw(map[string]any{"magicDNS": true, "dns": []any{"1.1.1.1", "9.9.9.9"}}),
	})

	out = append(out, tailscalePolicyAssets()...)
	return out
}

// tsRule mirrors one entry in the tailnet policy file. src/dst use the real
// selector language, which is what tailscaleACLFlow resolves.
type tsRule struct {
	kind, action, src, dst, proto string
}

var tsRules = []tsRule{
	// group:eng → every tag:prod device on port 22 (exercises Edge.Port).
	{"acl", "accept", "group:eng", "tag:prod:22", "tcp"},
	// Two tagged fleets talking to each other.
	{"acl", "accept", "tag:ci", "tag:lab:443", "tcp"},
	// A denial — traffic-deny edges only exist because a policy says "no".
	{"acl", "deny", "tag:ci", "tag:db:5432", "tcp"},
	// Named host alias as a destination.
	{"acl", "accept", "group:ops", "office-gw:443", "tcp"},
	// SSH check rule: an allow gated on re-auth.
	{"ssh", "check", "group:ops", "tag:bastion:22", ""},
	// A grant, which has no action field upstream and defaults to accept.
	{"grant", "accept", "group:eng", "tag:router", ""},
}

func tailscalePolicyAssets() []core.Asset {
	out := make([]core.Asset, 0, 16)

	out = append(out, core.Asset{
		Provider: "tailscale", AccountID: tailnetName,
		Type: "tailscale.acl", ID: "acl:" + tailnetName, Name: "Tailnet policy file", Status: "active",
		CreatedAt: created(916),
		Tags: tags("acl_rules", "4", "grants", "1", "ssh_rules", "1",
			"groups", "3", "tag_owners", "5", "hosts", "2"),
		Raw: raw(map[string]any{"acls": []any{}, "groups": map[string]any{}}),
	})

	for i, r := range tsRules {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.acl_rule",
			ID:   fmt.Sprintf("%s:acl/%s/%d", tailnetName, r.kind, i),
			Name: fmt.Sprintf("%s %d: %s → %s", r.kind, i, r.src, r.dst),
			// Status carries the verdict; core.TrafficEdgeKind maps it.
			Status:    r.action,
			CreatedAt: created(917 + i),
			Tags: tags("rule_kind", r.kind, "action", r.action, "src", r.src,
				"dst", r.dst, "proto", r.proto, "index", itoa(i)),
			Raw: raw(map[string]any{"action": r.action, "src": []any{r.src}, "dst": []any{r.dst}}),
		})
	}

	groups := []struct{ name, members string }{
		{"group:eng", "amara@northwind.example,chen@northwind.example,farid@northwind.example"},
		{"group:ops", "ops@northwind.example,amara@northwind.example"},
		{"group:labs", "labs@northwind.example"},
	}
	for i, g := range groups {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.acl_group", ID: tailnetName + ":group/" + g.name, Name: g.name,
			CreatedAt: created(925 + i),
			Tags:      tags("members", g.members, "member_count", itoa(len(strings.Split(g.members, ",")))),
			Raw:       raw(strings.Split(g.members, ",")),
		})
	}

	for i, t := range []string{"tag:prod", "tag:ci", "tag:db", "tag:lab", "tag:router", "tag:bastion"} {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.acl_tag", ID: tailnetName + ":tag/" + t, Name: t,
			CreatedAt: created(930 + i),
			Tags:      tags("owners", "group:ops", "owner_count", "1"),
			Raw:       raw([]string{"group:ops"}),
		})
	}

	hosts := []struct{ name, addr string }{
		{"office-gw", "198.51.100.7"},
		{"legacy-dc", "192.0.2.201"},
	}
	for i, h := range hosts {
		out = append(out, core.Asset{
			Provider: "tailscale", AccountID: tailnetName,
			Type: "tailscale.acl_host", ID: tailnetName + ":host/" + h.name, Name: h.name,
			CreatedAt: created(940 + i),
			Tags:      tags("ip", h.addr, "hostname", h.name),
			Raw:       raw(h.addr),
		})
	}

	return out
}

// ----------------------------------------------------------------------
// NetBird
// ----------------------------------------------------------------------

const (
	nbGroupServers = "g-srv-01hxr9"
	nbGroupClients = "g-cli-01hxr9"
	nbGroupAll     = "g-all-01hxr9"
	nbGroupCI      = "g-ci-01hxr9"
	nbGroupDB      = "g-db-01hxr9"
)

type nbPeer struct {
	id, name, ip, dnsLabel, os, groups string
}

var nbPeers = []nbPeer{
	{"p-01hxr9a001", "nb-gateway", nbGatewayIP, nbGatewayDNS, "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a002", "nb-app-01", "100.92.14.7", "nb-app-01.netbird.selfhosted", "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a003", "nb-app-02", "100.92.14.8", "nb-app-02.netbird.selfhosted", "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a004", "nb-db-01", "100.92.14.9", "nb-db-01.netbird.selfhosted", "linux", nbGroupDB + "," + nbGroupAll},
	{"p-01hxr9a005", "nb-db-02", "100.92.14.10", "nb-db-02.netbird.selfhosted", "linux", nbGroupDB + "," + nbGroupAll},
	{"p-01hxr9a006", "nb-ci-runner", "100.92.14.20", "nb-ci-runner.netbird.selfhosted", "linux", nbGroupCI + "," + nbGroupAll},
	{"p-01hxr9a007", "amara-laptop", "100.92.14.31", "amara-laptop.netbird.selfhosted", "darwin", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a008", "chen-laptop", "100.92.14.32", "chen-laptop.netbird.selfhosted", "linux", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a009", "dara-laptop", "100.92.14.33", "dara-laptop.netbird.selfhosted", "windows", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a010", "elif-laptop", "100.92.14.34", "elif-laptop.netbird.selfhosted", "darwin", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a011", "farid-laptop", "100.92.14.35", "farid-laptop.netbird.selfhosted", "linux", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a012", "gita-laptop", "100.92.14.36", "gita-laptop.netbird.selfhosted", "windows", nbGroupClients + "," + nbGroupAll},
	{"p-01hxr9a013", "nb-edge-fra", "100.92.14.41", "nb-edge-fra.netbird.selfhosted", "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a014", "nb-edge-lon", "100.92.14.42", "nb-edge-lon.netbird.selfhosted", "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a015", "nb-monitor", "100.92.14.50", "nb-monitor.netbird.selfhosted", "linux", nbGroupServers + "," + nbGroupAll},
	{"p-01hxr9a016", "nb-lab-pi", "100.92.14.60", "nb-lab-pi.netbird.selfhosted", "linux", nbGroupAll},
}

func netbirdAssets() []core.Asset {
	out := make([]core.Asset, 0, 48)

	out = append(out, core.Asset{
		Provider: "netbird", AccountID: netbirdAccountID,
		Type: "netbird.account", ID: netbirdAccountID, Name: "Northwind mesh", Status: "active",
		CreatedAt: created(950),
		Tags:      tags("peer_login_expiration_enabled", "true", "peer_login_expiration", "24h", "routing_peer_dns_resolution_enabled", "true"),
		Raw:       raw(map[string]any{"id": netbirdAccountID}),
	})

	for i, p := range nbPeers {
		status := "connected"
		if i%9 == 7 {
			status = "disconnected"
		}
		names := make([]string, 0, 2)
		for _, g := range strings.Split(p.groups, ",") {
			names = append(names, nbGroupName(g))
		}
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.peer", ID: p.id, Name: p.name, Status: status,
			CreatedAt: created(952 + i),
			Tags: tags(
				"ip", p.ip,
				"connection_ip", fmt.Sprintf("203.0.113.%d", 100+i),
				"hostname", p.name,
				"dns_label", p.dnsLabel,
				"os", p.os,
				"version", "0.30.2",
				"groups", strings.Join(names, ","),
				"group_ids", p.groups,
				"ssh_enabled", strconv.FormatBool(i%3 == 0),
				"login_expired", "false",
				"ephemeral", strconv.FormatBool(i == 15),
			),
			Raw: raw(map[string]any{"id": p.id, "name": p.name, "ip": p.ip, "dns_label": p.dnsLabel, "connected": status == "connected"}),
		})
	}

	groups := []struct{ id, name string }{
		{nbGroupAll, "All"},
		{nbGroupServers, "Servers"},
		{nbGroupClients, "Clients"},
		{nbGroupCI, "CI"},
		{nbGroupDB, "Databases"},
	}
	for i, g := range groups {
		count := 0
		for _, p := range nbPeers {
			if strings.Contains(p.groups, g.id) {
				count++
			}
		}
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.group", ID: g.id, Name: g.name,
			CreatedAt: created(975 + i),
			Tags:      tags("peers_count", itoa(count), "issued", "api"),
			Raw:       raw(map[string]any{"id": g.id, "name": g.name, "peers_count": count}),
		})
	}

	out = append(out, netbirdPolicyAssets()...)

	for i, k := range []string{"server-bootstrap", "laptop-enrolment", "ci-ephemeral"} {
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.setup_key", ID: fmt.Sprintf("sk-01hxr9%s", pad(i)), Name: k, Status: "valid",
			CreatedAt: created(990 + i),
			Tags:      tags("key_type", "reusable", "used_times", itoa(i*4), "ephemeral", strconv.FormatBool(i == 2), "auto_groups", nbGroupAll),
			Raw:       raw(map[string]any{"id": fmt.Sprintf("sk-01hxr9%s", pad(i)), "name": k}),
		})
	}

	nbUsers := []struct{ id, email, role string }{
		{"u-01hxr9a1", "ops@northwind.example", "owner"},
		{"u-01hxr9a2", "amara@northwind.example", "admin"},
		{"u-01hxr9a3", "chen@northwind.example", "user"},
		{"u-01hxr9a4", "dara@northwind.example", "user"},
		{"u-01hxr9a5", "elif@northwind.example", "user"},
	}
	for i, u := range nbUsers {
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.user", ID: u.id, Name: u.email, Status: "active",
			CreatedAt: created(995 + i),
			Tags:      tags("email", u.email, "role", u.role, "is_service_user", "false", "auto_groups", nbGroupClients),
			Raw:       raw(map[string]any{"id": u.id, "email": u.email, "role": u.role}),
		})
	}

	routes := []struct{ id, name, network, peer string }{
		{"r-01hxr9a1", "oci-prod-vcn", "10.20.0.0/16", "p-01hxr9a013"},
		{"r-01hxr9a2", "oci-plat-vcn", "10.40.0.0/16", "p-01hxr9a014"},
		{"r-01hxr9a3", "office-lan", "192.168.10.0/24", "p-01hxr9a001"},
	}
	for i, rt := range routes {
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.route", ID: rt.id, Name: rt.name, Status: "enabled",
			CreatedAt: created(1000 + i),
			Tags:      tags("network", rt.network, "network_type", "IPv4", "peer", rt.peer, "masquerade", "true", "metric", "9999"),
			Raw:       raw(map[string]any{"id": rt.id, "network": rt.network, "peer": rt.peer}),
		})
	}

	for i, ns := range []struct{ id, name, servers string }{
		{"ns-01hxr9a1", "Northwind internal DNS", "10.20.0.53,10.40.0.53"},
		{"ns-01hxr9a2", "Public resolvers", "1.1.1.1,9.9.9.9"},
	} {
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.nameserver", ID: ns.id, Name: ns.name, Status: "enabled",
			CreatedAt: created(1005 + i),
			Tags:      tags("nameservers", ns.servers, "domains", "northwind.internal", "primary", strconv.FormatBool(i == 0), "groups", nbGroupAll),
			Raw:       raw(map[string]any{"id": ns.id, "name": ns.name}),
		})
	}

	return out
}

func nbGroupName(id string) string {
	switch id {
	case nbGroupAll:
		return "All"
	case nbGroupServers:
		return "Servers"
	case nbGroupClients:
		return "Clients"
	case nbGroupCI:
		return "CI"
	case nbGroupDB:
		return "Databases"
	}
	return id
}

// netbirdPolicyAssets emit the policy + one asset per rule, which is what
// netbirdPolicyFlow hangs its edges on. One rule is bidirectional (the reverse
// path is drawn explicitly) and one is a drop (a traffic-deny edge).
func netbirdPolicyAssets() []core.Asset {
	type nbRule struct {
		policyID, policyName, ruleID, name string
		action, ports, sources, dests      string
		bidirectional                      bool
	}
	policies := []struct {
		id, name string
		enabled  bool
	}{
		{"pol-01hxr9a1", "Default mesh access", true},
		{"pol-01hxr9a2", "Database isolation", true},
		{"pol-01hxr9a3", "Lab sandbox (disabled)", false},
	}
	rules := []nbRule{
		{"pol-01hxr9a1", "Default mesh access", "rule-01", "clients to servers", "accept", "443", nbGroupClients, nbGroupServers, true},
		{"pol-01hxr9a1", "Default mesh access", "rule-02", "servers ssh", "accept", "22", nbGroupServers, nbGroupServers, false},
		{"pol-01hxr9a2", "Database isolation", "rule-01", "servers to databases", "accept", "5432", nbGroupServers, nbGroupDB, false},
		{"pol-01hxr9a2", "Database isolation", "rule-02", "block CI from databases", "drop", "5432", nbGroupCI, nbGroupDB, false},
		{"pol-01hxr9a3", "Lab sandbox (disabled)", "rule-01", "lab wide open", "accept", "", nbGroupAll, nbGroupAll, true},
	}

	out := make([]core.Asset, 0, len(policies)+len(rules))
	enabled := map[string]bool{}
	for i, p := range policies {
		enabled[p.id] = p.enabled
		status := "enabled"
		if !p.enabled {
			status = "disabled"
		}
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.policy", ID: p.id, Name: p.name, Status: status,
			CreatedAt: created(980 + i),
			Tags:      tags("enabled", strconv.FormatBool(p.enabled), "rules_count", "2"),
			Raw:       raw(map[string]any{"id": p.id, "name": p.name, "enabled": p.enabled}),
		})
	}
	for i, r := range rules {
		status := r.action
		if !enabled[r.policyID] {
			status = "disabled"
		}
		out = append(out, core.Asset{
			Provider: "netbird", AccountID: netbirdAccountID,
			Type: "netbird.policy_rule", ID: r.policyID + "/" + r.ruleID, Name: r.name, Status: status,
			CreatedAt: created(984 + i),
			Tags: tags(
				"policy_id", r.policyID,
				"policy_name", r.policyName,
				"action", r.action,
				"enabled", strconv.FormatBool(enabled[r.policyID]),
				"protocol", "tcp",
				"ports", r.ports,
				"bidirectional", strconv.FormatBool(r.bidirectional),
				"sources", r.sources,
				"destinations", r.dests,
				"source_names", nbGroupName(r.sources),
				"destination_names", nbGroupName(r.dests),
			),
			Raw: raw(map[string]any{"id": r.ruleID, "action": r.action, "bidirectional": r.bidirectional}),
		})
	}
	return out
}
