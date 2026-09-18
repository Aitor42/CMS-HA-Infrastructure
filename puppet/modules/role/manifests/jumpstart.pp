# role::jumpstart — Infrastructure provisioning, configuration management, and core services.
#
# Runs: Cobbler (PXE, DHCP, TFTP, HTTP), Puppet Server (8140), BIND9 DNS (53), Chrony NTP (123), NFS.
# Inherits role::base for base packages, /etc/hosts, swap, and common UFW rules.

class role::jumpstart {
  include role::base

  # Puppet Server (used by all nodes across Internal and Main subnets)
  exec { 'ufw-allow-puppetserver':
    command => '/usr/sbin/ufw allow 8140/tcp comment "Puppet Server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "8140/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # Cobbler HTTP & HTTPS
  exec { 'ufw-allow-http':
    command => '/usr/sbin/ufw allow 80/tcp comment "HTTP Cobbler/Apt"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "80/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-allow-https':
    command => '/usr/sbin/ufw allow 443/tcp comment "HTTPS Cobbler"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "443/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # Cobbler XML-RPC
  exec { 'ufw-allow-cobbler-xmlrpc':
    command => '/usr/sbin/ufw allow 25151/tcp comment "Cobbler XML-RPC"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "25151/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # TFTP for PXE booting
  exec { 'ufw-allow-tftp':
    command => '/usr/sbin/ufw allow 69/udp comment "TFTP PXE"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "69/udp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # DHCP server
  exec { 'ufw-allow-dhcp':
    command => '/usr/sbin/ufw allow 67/udp comment "DHCP Server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "67/udp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # DNS server (BIND9)
  exec { 'ufw-allow-dns-udp':
    command => '/usr/sbin/ufw allow 53/udp comment "DNS Server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "53/udp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-allow-dns-tcp':
    command => '/usr/sbin/ufw allow 53/tcp comment "DNS Server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "53/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # NTP server (Chrony)
  exec { 'ufw-allow-ntp':
    command => '/usr/sbin/ufw allow 123/udp comment "NTP Server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "123/udp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  # NFS server
  exec { 'ufw-allow-nfs':
    command => '/usr/sbin/ufw allow 2049 comment "NFS"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2049.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-allow-rpcbind':
    command => '/usr/sbin/ufw allow 111 comment "RPCbind"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "111.*ALLOW"',
    require => Exec['ufw-enable'],
  }
}
