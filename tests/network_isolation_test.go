package tests

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNetworkIsolationNFSv4Only(t *testing.T) {
	storage := readRepoFile(t, "puppet/modules/role/manifests/storage.pp")
	frontend := readRepoFile(t, "puppet/modules/role/manifests/cms_frontend.pp")
	if !strings.Contains(frontend, "nfsvers=4") {
		t.Fatal("NFS firewall assumes that CMS mounts explicitly use NFSv4")
	}
	nfs := puppetResourceBlock(t, storage, `exec { "ufw-storage-nfs-${ip}":`)
	for _, want := range []string{
		"allow from ${ip} to any port 2049 proto tcp",
		"require => Exec['ufw-enable']",
	} {
		if !strings.Contains(nfs, want) {
			t.Errorf("NFS rule missing %q", want)
		}
	}
	for _, forbidden := range []string{"ufw allow 2049", "ufw allow 111", "exec { 'ufw-storage-rpcbind':", "no_root_squash"} {
		if strings.Contains(storage, forbidden) {
			t.Errorf("storage retains unsafe/unnecessary rule %q", forbidden)
		}
	}
	for _, want := range []string{
		"lookup('cms_ha::cms_frontend_ips', Array[String], undef, [])",
		"if $cms_frontend_ips == []",
		"fail('cms_ha::cms_frontend_ips is empty:",
		"$nfs_clients = $cms_frontend_ips.map |$ip|",
		"${ip}(rw,sync,no_subtree_check,all_squash,anonuid=33,anongid=33)",
		`content => "/srv/nfs/wp-uploads ${nfs_clients}\n"`,
	} {
		if !strings.Contains(storage, want) {
			t.Errorf("frontend-only NFS export missing %q", want)
		}
	}
	if strings.Contains(storage, "${main_cidr}(") {
		t.Error("NFS exports must no longer authorize all Main clients/hotdesks")
	}
	cleanup := puppetResourceBlock(t, storage, `exec { "ufw-storage-remove-legacy-${port}":`)
	for _, want := range []string{
		"$legacy_nfs_rules = { '2049' => '2049', '2049/tcp' => '2049 proto tcp', '111' => '111' }",
		"--force delete allow from ${main_cidr} to any port ${arguments}",
		`$nfs_firewall_rules = $cms_frontend_ips.map |$ip| { Exec["ufw-storage-nfs-${ip}"] }`,
		"require => $nfs_firewall_rules",
	} {
		if !strings.Contains(storage, want) {
			t.Errorf("NFS upgrade missing %q", want)
		}
	}
	if strings.Contains(cleanup, "|| true") {
		t.Error("NFS migration must not hide deletion failures")
	}
}

func TestNetworkIsolationK3sSourceBoundsAndUpgrade(t *testing.T) {
	master := readRepoFile(t, "puppet/modules/role/manifests/k3s_master.pp")
	api := puppetResourceBlock(t, master, "exec { 'ufw-k3s-api':")
	if !strings.Contains(api, "allow from ${internal_cidr} to any port 6443 proto tcp") {
		t.Error("K3s API must be scoped to the configured Internal network")
	}
	for _, want := range []string{
		"lookup('cms_ha::main_cidr'",
		"$database_sources = { 'main' => $main_cidr, 'internal' => $internal_cidr }",
		"allow from ${source} to any port 30306 proto tcp",
		"[6443, 30306].each",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("K3s source bounds missing %q", want)
		}
	}
	for _, line := range strings.Split(master, "\n") {
		if strings.Contains(line, "command =>") && strings.Contains(line, "/usr/sbin/ufw allow ") &&
			!strings.Contains(line, "allow from ") {
			t.Errorf("K3s still creates a public grant: %s", line)
		}
	}
	cleanup := puppetResourceBlock(t, master, `exec { "ufw-k3s-remove-public-${port}":`)
	for _, want := range []string{
		"--force delete allow ${port}/tcp",
		"Exec['ufw-k3s-api']",
		"Exec['ufw-mariadb-nodeport-main']",
		"Exec['ufw-mariadb-nodeport-internal']",
	} {
		if !strings.Contains(cleanup, want) {
			t.Errorf("public-rule migration missing %q", want)
		}
	}
	if strings.Contains(cleanup, "|| true") {
		t.Error("public-rule migration must not hide deletion failures")
	}
}

func TestNetworkIsolationPreservesBootstrapAndHealthcheckPaths(t *testing.T) {
	master := readRepoFile(t, "puppet/modules/role/manifests/k3s_master.pp")
	base := readRepoFile(t, "puppet/modules/role/manifests/base.pp")
	for _, want := range []string{
		"allow from ${internal_cidr} to any port 2379:2380 proto tcp",
		"allow from ${monitor_ip} to any port 2381 proto tcp",
		"allow from ${internal_cidr} to any port 10250 proto tcp",
		"allow from ${internal_cidr} to any port 8472 proto udp",
		"allow from ${internal_cidr} to any port 7788 proto tcp",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("existing cluster/monitoring path changed: %q", want)
		}
	}
	if !strings.Contains(base, "ufw allow 22/tcp") {
		t.Error("hypervisor SSH/bootstrap access must remain available")
	}
	for _, manifest := range []string{master, readRepoFile(t, "puppet/modules/role/manifests/storage.pp")} {
		for _, forbidden := range []string{"ufw reset", "ufw default deny outgoing", "iptables -F"} {
			if strings.Contains(manifest, forbidden) {
				t.Errorf("network hardening must not flush bootstrap/cluster rules: %s", forbidden)
			}
		}
	}
	// The exporter is local, not a reason to open SQL to the monitor IP alone.
	exporter := readRepoFile(t, "puppet/modules/role/files/cms-ha/configure-mysqld-exporter.py")
	if !strings.Contains(exporter, `host=127.0.0.1\nport=30306`) {
		t.Error("exporter consumer changed; revisit database firewall sources")
	}
	for _, path := range []string{"internal/verify/verify_services.go", "internal/phases/traffic/traffic.go"} {
		if !strings.Contains(readRepoFile(t, path), "dialTCP(ctx,") {
			t.Errorf("%s: revisit the orchestrator's direct NodePort healthcheck allowance", path)
		}
	}
}

// Exercise the actual shell guards against UFW output without invoking UFW,
// root privileges, network access, or an infrastructure deployment.
func TestNetworkIsolationUFWGuards(t *testing.T) {
	storage := readRepoFile(t, "puppet/modules/role/manifests/storage.pp")
	master := readRepoFile(t, "puppet/modules/role/manifests/k3s_master.pp")
	tests := []struct {
		name, manifest, header, attribute, status string
		values                                    map[string]string
		matches                                   bool
	}{
		{"nfs frontend", storage, `exec { "ufw-storage-nfs-${ip}":`, "unless", "2049/tcp  ALLOW  10.20.0.101  # NFSv4", map[string]string{"ip": "10.20.0.101"}, true},
		{"nfs old subnet tcp", storage, `exec { "ufw-storage-nfs-${ip}":`, "unless", "2049/tcp  ALLOW  10.20.0.0/24  # NFSv4", map[string]string{"ip": "10.20.0.101"}, false},
		{"nfs old both protocols", storage, `exec { "ufw-storage-nfs-${ip}":`, "unless", "2049  ALLOW  10.20.0.0/24  # NFS", map[string]string{"ip": "10.20.0.101"}, false},
		{"nfs hotdesk", storage, `exec { "ufw-storage-nfs-${ip}":`, "unless", "2049/tcp  ALLOW  10.20.0.201  # hotdesk", map[string]string{"ip": "10.20.0.101"}, false},
		{"nfs wrong source prefix", storage, `exec { "ufw-storage-nfs-${ip}":`, "unless", "2049/tcp  ALLOW  110.20.0.101  # NFSv4", map[string]string{"ip": "10.20.0.101"}, false},
		{"api scoped", master, "exec { 'ufw-k3s-api':", "unless", "6443/tcp  ALLOW  10.10.0.0/24  # K3s", map[string]string{"internal_cidr": "10.10.0.0/24"}, true},
		{"api public does not satisfy scoped guard", master, "exec { 'ufw-k3s-api':", "unless", "6443/tcp  ALLOW  Anywhere  # K3s", map[string]string{"internal_cidr": "10.10.0.0/24"}, false},
		{"api wrong source", master, "exec { 'ufw-k3s-api':", "unless", "6443/tcp  ALLOW  10.20.0.0/24  # K3s", map[string]string{"internal_cidr": "10.10.0.0/24"}, false},
		{"database scoped", master, `exec { "ufw-mariadb-nodeport-${zone}":`, "unless", "30306/tcp  ALLOW  10.20.0.0/24  # MariaDB", map[string]string{"source": "10.20.0.0/24"}, true},
		{"database public does not satisfy scoped guard", master, `exec { "ufw-mariadb-nodeport-${zone}":`, "unless", "30306/tcp  ALLOW  Anywhere  # MariaDB", map[string]string{"source": "10.20.0.0/24"}, false},
		{"database other zone", master, `exec { "ufw-mariadb-nodeport-${zone}":`, "unless", "30306/tcp  ALLOW  10.10.0.0/24  # MariaDB", map[string]string{"source": "10.20.0.0/24"}, false},
		{"delete old nfs", storage, `exec { "ufw-storage-remove-legacy-${port}":`, "onlyif", "2049  ALLOW  10.20.0.0/24  # NFS", map[string]string{"port": "2049", "main_cidr": "10.20.0.0/24"}, true},
		{"delete old rpcbind", storage, `exec { "ufw-storage-remove-legacy-${port}":`, "onlyif", "111  ALLOW  10.20.0.0/24  # RPCbind", map[string]string{"port": "111", "main_cidr": "10.20.0.0/24"}, true},
		{"delete old subnet tcp", storage, `exec { "ufw-storage-remove-legacy-${port}":`, "onlyif", "2049/tcp  ALLOW  10.20.0.0/24  # NFSv4", map[string]string{"port": "2049/tcp", "main_cidr": "10.20.0.0/24"}, true},
		{"preserve nfs frontend rule", storage, `exec { "ufw-storage-remove-legacy-${port}":`, "onlyif", "2049/tcp  ALLOW  10.20.0.101  # NFSv4", map[string]string{"port": "2049/tcp", "main_cidr": "10.20.0.0/24"}, false},
		{"delete public api v4", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "6443/tcp  ALLOW  Anywhere  # K3s", map[string]string{"port": "6443"}, true},
		{"delete public api v6", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "6443/tcp (v6)  ALLOW  Anywhere (v6)  # K3s", map[string]string{"port": "6443"}, true},
		{"delete public database v4", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "30306/tcp  ALLOW  Anywhere  # MariaDB", map[string]string{"port": "30306"}, true},
		{"delete public database v6", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "30306/tcp (v6)  ALLOW  Anywhere (v6)  # MariaDB", map[string]string{"port": "30306"}, true},
		{"preserve scoped api", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "6443/tcp  ALLOW  10.10.0.0/24  # K3s", map[string]string{"port": "6443"}, false},
		{"preserve scoped database", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "30306/tcp  ALLOW  10.20.0.0/24  # MariaDB", map[string]string{"port": "30306"}, false},
		{"already converged", master, `exec { "ufw-k3s-remove-public-${port}":`, "onlyif", "Status: active", map[string]string{"port": "6443"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := puppetResourceBlock(t, tt.manifest, tt.header)
			var guard string
			for _, line := range strings.Split(block, "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), "=>")
				if ok && strings.TrimSpace(key) == tt.attribute {
					guard = strings.TrimSuffix(strings.TrimSpace(value), ",")
					guard = strings.Trim(guard, `"`)
					break
				}
			}
			if guard == "" {
				t.Fatalf("missing %s guard in %s", tt.attribute, tt.header)
			}
			for key, value := range tt.values {
				guard = strings.ReplaceAll(guard, "${"+key+"}", value)
			}
			if strings.Contains(guard, "${") {
				t.Fatalf("unresolved Puppet interpolation: %s", guard)
			}
			guard = strings.ReplaceAll(guard, "/usr/sbin/ufw status", "printf '%s\\n' '"+strings.ReplaceAll(tt.status, "'", "'\\''")+"'")
			if strings.Contains(guard, "/usr/sbin/ufw") {
				t.Fatal("test must never invoke the real firewall")
			}
			err := exec.Command("/bin/sh", "-c", guard).Run()
			if err != nil {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
					t.Fatalf("guard execution failed: %v", err)
				}
			}
			if got := err == nil; got != tt.matches {
				t.Errorf("guard match = %v, want %v for %q", got, tt.matches, tt.status)
			}
		})
	}
}
