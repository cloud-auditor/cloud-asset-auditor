package tailscale

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// device is the subset of GET /tailnet/{tailnet}/devices we map. The address
// fields (addresses / name / hostname) are the network identifiers a topology
// resolver joins on, so they're surfaced as tags rather than hidden in Raw —
// the join then works on a plain audit snapshot, without --include-raw.
//
// machineKey and nodeKey are deliberately absent: they're node secrets, and
// omitting the struct fields means they can never reach Asset.Raw even when
// --include-raw is set (same belt-and-braces rule as NetBird's setup_key.key).
type device struct {
	ID                        string   `json:"id"`
	NodeID                    string   `json:"nodeId"`
	Name                      string   `json:"name"`
	Hostname                  string   `json:"hostname"`
	User                      string   `json:"user"`
	Addresses                 []string `json:"addresses"`
	OS                        string   `json:"os"`
	ClientVersion             string   `json:"clientVersion"`
	UpdateAvailable           bool     `json:"updateAvailable"`
	Created                   string   `json:"created"`
	LastSeen                  string   `json:"lastSeen"`
	ConnectedToControl        bool     `json:"connectedToControl"`
	Expires                   string   `json:"expires"`
	KeyExpiryDisabled         bool     `json:"keyExpiryDisabled"`
	Authorized                bool     `json:"authorized"`
	IsExternal                bool     `json:"isExternal"`
	IsEphemeral               bool     `json:"isEphemeral"`
	BlocksIncomingConnections bool     `json:"blocksIncomingConnections"`
	SSHEnabled                bool     `json:"sshEnabled"`
	Tags                      []string `json:"tags"`
	EnabledRoutes             []string `json:"enabledRoutes"`
	AdvertisedRoutes          []string `json:"advertisedRoutes"`
	TailnetLockError          string   `json:"tailnetLockError"`
	Distro                    struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"distro"`
}

type deviceList struct {
	Devices []device `json:"devices"`
}

// collectDevices lists the tailnet's devices. `fields=all` is requested so
// the subnet-route fields (enabledRoutes / advertisedRoutes) come back — they
// are omitted from the default field set, and they're what makes a subnet
// router visible as a gateway in the topology graph.
func (p *Provider) collectDevices(ctx context.Context, out chan<- core.Asset) error {
	var list deviceList
	if err := p.client.getJSON(ctx, p.tailnetPath("/devices?fields=all"), &list); err != nil {
		return err
	}
	for _, d := range list.Devices {
		if !sendAsset(ctx, out, p.deviceToAsset(d)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) deviceToAsset(d device) core.Asset {
	// nodeId is the identifier every other endpoint takes; the legacy
	// numeric id is the fallback for older control planes that omit it.
	id := d.NodeID
	if id == "" {
		id = d.ID
	}
	name := d.Name
	if name == "" {
		name = d.Hostname
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.device",
		ID:        id,
		Name:      name,
		Status:    connectedStatus(d.ConnectedToControl),
		CreatedAt: parseTime(d.Created),
		Tags: tagsOf(
			"ip", firstIPv4(d.Addresses),
			"ipv6", firstIPv6(d.Addresses),
			"addresses", joinStr(d.Addresses),
			"dns_name", d.Name,
			"hostname", d.Hostname,
			"user", d.User,
			"os", d.OS,
			"distro", d.Distro.Name,
			"distro_version", d.Distro.Version,
			"client_version", d.ClientVersion,
			"update_available", boolStr(d.UpdateAvailable),
			"authorized", boolStr(d.Authorized),
			"external", boolStr(d.IsExternal),
			"ephemeral", boolStr(d.IsEphemeral),
			"ssh_enabled", boolStr(d.SSHEnabled),
			"blocks_incoming", boolStr(d.BlocksIncomingConnections),
			"key_expiry_disabled", boolStr(d.KeyExpiryDisabled),
			"expires", d.Expires,
			"last_seen", d.LastSeen,
			"acl_tags", joinStr(d.Tags),
			"enabled_routes", joinStr(d.EnabledRoutes),
			"advertised_routes", joinStr(d.AdvertisedRoutes),
			"tailnet_lock_error", d.TailnetLockError,
			"legacy_id", d.ID,
		),
		Raw: p.rawOf(d),
	}
}
