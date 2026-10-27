package config

import (
	"fmt"
	"strings"
)

// NodeSpec is a flattened representation of a node
type NodeSpec struct {
	Name         string
	IP           string
	FQDN         string
	MAC          string
	MACWAN       string
	RAMInstallMB int
	RAMFinalMB   int
	VCPUs        int
	DiskGB       int
}

func toSpec(n NodeDetail, fallbackIP string) NodeSpec {
	ip := n.IP
	if ip == "" {
		ip = fallbackIP
	}
	mac := n.MAC
	if mac == "" && n.MACInternal != "" {
		mac = n.MACInternal
	} else if mac == "" && n.MACMain != "" {
		mac = n.MACMain
	}
	return NodeSpec{
		Name:         n.Name,
		IP:           ip,
		FQDN:         n.FQDN,
		MAC:          mac,
		MACWAN:       n.MACWAN,
		RAMInstallMB: n.RAMInstallMB,
		RAMFinalMB:   n.RAMFinalMB,
		VCPUs:        n.VCPUs,
		DiskGB:       n.DiskGB,
	}
}

// InternalNodes returns the nodes on the internal network.
func (c *Config) InternalNodes() []NodeSpec {
	var nodes []NodeSpec
	nodes = append(nodes, toSpec(c.Nodes.Router, ""))
	nodes = append(nodes, toSpec(c.Nodes.Jumpstart, c.Nodes.Jumpstart.IP))
	for _, n := range c.Nodes.Masters {
		nodes = append(nodes, toSpec(n, ""))
	}
	for _, n := range c.Nodes.Workers {
		nodes = append(nodes, toSpec(n, ""))
	}
	nodes = append(nodes, toSpec(c.Nodes.Storage, ""))
	nodes = append(nodes, toSpec(c.Nodes.Monitor, ""))
	return nodes
}

// MainNodes returns the nodes on the main network.
func (c *Config) MainNodes() []NodeSpec {
	var nodes []NodeSpec
	if c.Nodes.Jumpstart.IPMain != "" {
		js := toSpec(c.Nodes.Jumpstart, c.Nodes.Jumpstart.IPMain)
		js.MAC = c.Nodes.Jumpstart.MACMain
		nodes = append(nodes, js)
	}
	for _, lb := range c.Nodes.LBs {
		nodes = append(nodes, toSpec(lb, ""))
	}
	for _, n := range c.Nodes.CMSFrontends {
		nodes = append(nodes, toSpec(n, ""))
	}
	nodes = append(nodes, c.HotdeskSpecs()...)
	return nodes
}

// HotdeskSpecs synthesises the hotdesk nodes from Nodes.Hotdesks.Count and
// BaseIPOctet.
func (c *Config) HotdeskSpecs() []NodeSpec {
	// Hotdesks live on the main network, so derive their address range from the
	// configured CIDR instead of hardcoding 192.168.20.0/24.
	prefix := PrefixFromCIDR(c.Network.Main.CIDR, "192.168.20.")
	var nodes []NodeSpec
	for i := 1; i <= c.Nodes.Hotdesks.Count; i++ {
		lastOctet := 200 + i
		if c.Nodes.Hotdesks.BaseIPOctet > 0 && c.Nodes.Hotdesks.BaseIPOctet != 201 {
			lastOctet = c.Nodes.Hotdesks.BaseIPOctet + i - 1
		}
		nodes = append(nodes, NodeSpec{
			Name:         fmt.Sprintf("main-hotdesk%d", i),
			IP:           fmt.Sprintf("%s%d", prefix, lastOctet),
			FQDN:         fmt.Sprintf("main-hotdesk%d.main.local", i),
			MAC:          fmt.Sprintf("52:54:00:10:02:%02x", lastOctet),
			RAMInstallMB: c.Nodes.Hotdesks.RAMInstallMB,
			RAMFinalMB:   c.Nodes.Hotdesks.RAMFinalMB,
			VCPUs:        c.Nodes.Hotdesks.VCPUs,
			DiskGB:       c.Nodes.Hotdesks.DiskGB,
		})
	}
	return nodes
}

// PrefixFromCIDR returns the first three octets of an IPv4 CIDR plus the
// trailing dot (e.g. "192.168.20."), so callers can compose static host IPs
// in that range. Returns fallback for malformed CIDRs.
func PrefixFromCIDR(cidr, fallback string) string {
	parts := strings.Split(cidr, ".")
	if len(parts) >= 3 {
		return fmt.Sprintf("%s.%s.%s.", parts[0], parts[1], parts[2])
	}
	return fallback
}

// PrimaryLB returns the first load balancer node (the keepalived MASTER
// candidate). Returns a zero NodeDetail when none is configured.
func (c *Config) PrimaryLB() NodeDetail {
	if len(c.Nodes.LBs) == 0 {
		return NodeDetail{}
	}
	return c.Nodes.LBs[0]
}

// FirstMaster returns the first master with an address. Verification, drills
// and failover all need one reachable control-plane node and previously kept
// their own copy of this loop.
func (c *Config) FirstMaster() (NodeDetail, bool) {
	for _, m := range c.Nodes.Masters {
		if m.IP != "" {
			return m, true
		}
	}
	return NodeDetail{}, false
}

// LBVIP returns the keepalived floating address clients (and the router DNAT)
// use to reach the CMS.
func (c *Config) LBVIP() string {
	return c.Network.Main.VIP
}

// AllNodes returns all nodes across both networks
func (c *Config) AllNodes() []NodeSpec {
	var all []NodeSpec
	all = append(all, c.InternalNodes()...)
	// Don't duplicate Jumpstart
	for _, n := range c.MainNodes() {
		if n.Name != c.Nodes.Jumpstart.Name {
			all = append(all, n)
		}
	}
	return all
}

// AllNodeIPs returns every configured node IP, plus the jumpstart's
// main-network address.
func (c *Config) AllNodeIPs() []string {
	var ips []string
	for _, n := range c.AllNodes() {
		if n.IP != "" {
			ips = append(ips, n.IP)
		}
	}
	if c.Nodes.Jumpstart.IPMain != "" {
		ips = append(ips, c.Nodes.Jumpstart.IPMain)
	}
	return ips
}
