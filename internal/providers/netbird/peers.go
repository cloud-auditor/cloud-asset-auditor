package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// peer is the subset of GET /api/peers we map. The address fields (ip / ipv6 /
// connection_ip / dns_label / hostname) are the network identifiers a topology
// resolver joins on, so they're surfaced as tags.
type peer struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	IP            string     `json:"ip"`
	IPv6          string     `json:"ipv6"`
	ConnectionIP  string     `json:"connection_ip"`
	Connected     bool       `json:"connected"`
	LastSeen      string     `json:"last_seen"`
	OS            string     `json:"os"`
	KernelVersion string     `json:"kernel_version"`
	Version       string     `json:"version"`
	UIVersion     string     `json:"ui_version"`
	Groups        []groupRef `json:"groups"`
	SSHEnabled    bool       `json:"ssh_enabled"`
	UserID        string     `json:"user_id"`
	Hostname      string     `json:"hostname"`
	DNSLabel      string     `json:"dns_label"`
	SerialNumber  string     `json:"serial_number"`
	CountryCode   string     `json:"country_code"`
	CityName      string     `json:"city_name"`
	LoginExpired  bool       `json:"login_expired"`
	LastLogin     string     `json:"last_login"`
	Ephemeral     bool       `json:"ephemeral"`
}

func (p *Provider) collectPeers(ctx context.Context, out chan<- core.Asset) error {
	var peers []peer
	if err := p.client.getJSON(ctx, "/api/peers", &peers); err != nil {
		return err
	}
	for _, pr := range peers {
		if !sendAsset(ctx, out, p.peerToAsset(pr)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) peerToAsset(pr peer) core.Asset {
	name := pr.Name
	if name == "" {
		name = pr.Hostname
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.peer",
		ID:        pr.ID,
		Name:      name,
		Status:    connectedStatus(pr.Connected),
		Tags: tagsOf(
			"ip", pr.IP,
			"ipv6", pr.IPv6,
			"connection_ip", pr.ConnectionIP,
			"hostname", pr.Hostname,
			"dns_label", pr.DNSLabel,
			"os", pr.OS,
			"kernel_version", pr.KernelVersion,
			"version", pr.Version,
			"ui_version", pr.UIVersion,
			"groups", groupNames(pr.Groups),
			"group_ids", groupIDs(pr.Groups),
			"ssh_enabled", boolStr(pr.SSHEnabled),
			"user_id", pr.UserID,
			"serial_number", pr.SerialNumber,
			"country_code", pr.CountryCode,
			"city_name", pr.CityName,
			"login_expired", boolStr(pr.LoginExpired),
			"ephemeral", boolStr(pr.Ephemeral),
			"last_seen", pr.LastSeen,
			"last_login", pr.LastLogin,
		),
		Raw: p.rawOf(pr),
	}
}
