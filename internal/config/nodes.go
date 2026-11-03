package config

import (
	"encoding/binary"
	"fmt"
	"net"
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
	// Hotdesks live on the main network, so derive their address range and DNS
	// domain from the configured network instead of hardcoding 192.168.20.0/24
	// and main.local.
	prefix := PrefixFromCIDR(c.Network.Main.CIDR, "192.168.20.")
	domain := strings.TrimSpace(c.Network.Main.Domain)
	if domain == "" {
		domain = "main.local"
	}
	var nodes []NodeSpec
	for i := 1; i <= c.Nodes.Hotdesks.Count; i++ {
		lastOctet := 200 + i
		if c.Nodes.Hotdesks.BaseIPOctet > 0 && c.Nodes.Hotdesks.BaseIPOctet != 201 {
			lastOctet = c.Nodes.Hotdesks.BaseIPOctet + i - 1
		}
		nodes = append(nodes, NodeSpec{
			Name:         fmt.Sprintf("main-hotdesk%d", i),
			IP:           fmt.Sprintf("%s%d", prefix, lastOctet),
			FQDN:         fmt.Sprintf("main-hotdesk%d.%s", i, domain),
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

// UsableHostRange returns the first and last usable IPv4 host addresses of an
// IPv4 CIDR, together with its prefix length. The lab derives the hypervisor
// bridge address and the guest network masks from it, so a non-/24 topology
// does not silently fall back to the default /24.
func UsableHostRange(cidr string) (first, last string, prefix int, err error) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return "", "", 0, fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", "", 0, fmt.Errorf("CIDR %q is not IPv4", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", "", 0, fmt.Errorf("CIDR %q has no usable host addresses", cidr)
	}
	network := binary.BigEndian.Uint32(ip)
	mask := binary.BigEndian.Uint32(ipnet.Mask)
	broadcast := network | ^mask
	return formatIPv4(network + 1), formatIPv4(broadcast - 1), ones, nil
}

func formatIPv4(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
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
