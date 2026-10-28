# role::jumpstart — Infrastructure provisioning, configuration management, and core services.
#
# Runs: Cobbler (PXE, DHCP, TFTP, HTTP), Puppet Server (8140), BIND9 DNS (53), Chrony NTP (123), NFS.
# Includes role::base for base packages, /etc/hosts, swap, and common UFW rules.

class role::jumpstart {
  include role::base

  $internal_cidr = '192.168.10.0/24'
  $main_cidr = '192.168.20.0/24'
  $provisioning_sources = [$internal_cidr, $main_cidr]

  # Older releases opened the provisioning services to any source. Remove
  # those broad rules before adding the scoped ones; otherwise an upgraded
  # jumpstart would keep the WAN exposure it had before this change.
  exec { 'ufw-remove-public-provisioning-rules':
    command => '/bin/bash -c \'for rule in 8140/tcp 80/tcp 443/tcp 25151/tcp 69/udp 67/udp 53/udp 53/tcp 123/udp 2049 111; do /usr/sbin/ufw delete allow "$rule" >/dev/null 2>&1 || true; done\'',
    onlyif  => '/usr/sbin/ufw status | /usr/bin/grep -Eq "(8140/tcp|80/tcp|443/tcp|25151/tcp|69/udp|67/udp|53/udp|53/tcp|123/udp|2049|111)[[:space:]].*ALLOW[[:space:]]+Anywhere"',
    require => Exec['ufw-enable'],
  }

  # Puppet Server, Cobbler HTTP/HTTPS/XML-RPC. These are only needed by the
  # internal and main lab subnets, never from the WAN interface.
  $tcp_services = [
    { 'port' => '8140', 'comment' => 'Puppet Server' },
    { 'port' => '80', 'comment' => 'HTTP Cobbler/Apt' },
    { 'port' => '443', 'comment' => 'HTTPS Cobbler' },
    { 'port' => '25151', 'comment' => 'Cobbler XML-RPC' },
  ]
  $tcp_services.each |$service| {
    $provisioning_sources.each |$source| {
      $slug = regsubst("${service['port']}-${source}", '[^A-Za-z0-9]', '-', 'G')
      exec { "ufw-allow-${slug}":
        command => "/usr/sbin/ufw allow from ${source} to any port ${service['port']} proto tcp comment \"${service['comment']} from ${source}\"",
        unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"${service['port']}/tcp.*${source}\"",
        require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
      }
    }
  }

  # TFTP, DNS, and NTP are UDP services used by the provisioning clients.
  $udp_services = [
    { 'port' => '69', 'comment' => 'TFTP PXE' },
    { 'port' => '53', 'comment' => 'DNS Server' },
    { 'port' => '123', 'comment' => 'NTP Server' },
  ]
  $udp_services.each |$service| {
    $provisioning_sources.each |$source| {
      $slug = regsubst("${service['port']}-${source}", '[^A-Za-z0-9]', '-', 'G')
      exec { "ufw-allow-${slug}":
        command => "/usr/sbin/ufw allow from ${source} to any port ${service['port']} proto udp comment \"${service['comment']} from ${source}\"",
        unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"${service['port']}/udp.*${source}\"",
        require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
      }
    }
  }

  # DNS also answers TCP queries.
  $provisioning_sources.each |$source| {
    $slug = regsubst("53-tcp-${source}", '[^A-Za-z0-9]', '-', 'G')
    exec { "ufw-allow-${slug}":
      command => "/usr/sbin/ufw allow from ${source} to any port 53 proto tcp comment \"DNS Server from ${source}\"",
      unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"53/tcp.*${source}\"",
      require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
    }
  }

  # NFS and RPCbind are only consumed by the main-network CMS nodes.
  $any_services = [
    { 'port' => '2049', 'comment' => 'NFS' },
    { 'port' => '111', 'comment' => 'RPCbind' },
  ]
  $any_services.each |$service| {
    $provisioning_sources.each |$source| {
      $slug = regsubst("${service['port']}-${source}", '[^A-Za-z0-9]', '-', 'G')
      exec { "ufw-allow-${slug}":
        command => "/usr/sbin/ufw allow from ${source} to any port ${service['port']} comment \"${service['comment']} from ${source}\"",
        unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"${service['port']}.*${source}\"",
        require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
      }
    }
  }

  # DHCP clients send from 0.0.0.0, so a source-CIDR rule cannot match them.
  # Scope DHCP to the two provisioning interfaces instead of opening 67/udp
  # on the WAN interface (enp3s0).
  exec { 'ufw-allow-dhcp-internal':
    command => '/usr/sbin/ufw allow in on enp1s0 to any port 67 proto udp comment "DHCP internal"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "67/udp on enp1s0.*ALLOW"',
    require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
  }

  exec { 'ufw-allow-dhcp-main':
    command => '/usr/sbin/ufw allow in on enp2s0 to any port 67 proto udp comment "DHCP main"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "67/udp on enp2s0.*ALLOW"',
    require => [Exec['ufw-enable'], Exec['ufw-remove-public-provisioning-rules']],
  }

  # Internal CA (step-ca) — the load balancers (main network) renew their TLS
  # certificates against it. Restricted to the two lab subnets; never public.
  exec { 'ufw-allow-step-ca-internal':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 8443 proto tcp comment \"step-ca internal\"",
    unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"8443/tcp.*${internal_cidr}\"",
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-allow-step-ca-main':
    command => "/usr/sbin/ufw allow from ${main_cidr} to any port 8443 proto tcp comment \"step-ca main\"",
    unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"8443/tcp.*${main_cidr}\"",
    require => Exec['ufw-enable'],
  }
}
