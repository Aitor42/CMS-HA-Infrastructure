# role::storage — Centralised NFS storage server (internal-storage node).
#
# Manages: nfs-kernel-server, shared WordPress uploads directory,
# NFS exports for CMS frontend IPs, and UFW firewall rules.

class role::storage {

  include role::base

  # Main is only used to remove the old subnet-wide firewall grants.
  $main_cidr = lookup('cms_ha::main_cidr', String, undef, '192.168.20.0/24')
  $cms_frontend_ips = lookup('cms_ha::cms_frontend_ips', Array[String], undef, [])
  if $cms_frontend_ips == [] {
    fail('cms_ha::cms_frontend_ips is empty: generate Hiera with `cms-ha phase setup-puppet` before configuring NFS')
  }
  # Literal IPv4 clients only: never turn a CIDR, hostname or wildcard into an
  # export selector or shell argument, even when Hiera is edited manually.
  $cms_frontend_ips.each |$ip| {
    if $ip !~ /\A(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\z/ {
      fail("cms_ha::cms_frontend_ips must contain literal IPv4 addresses, got ${ip}")
    }
  }
  $nfs_clients = $cms_frontend_ips.map |$ip| {
    "${ip}(rw,sync,no_subtree_check,all_squash,anonuid=33,anongid=33)"
  }.join(' ')

  # ---------------------------------------------------------------------------
  # PACKAGES
  # ---------------------------------------------------------------------------
  package { 'nfs-kernel-server':
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # DIRECTORIES & PERMISSIONS
  # ---------------------------------------------------------------------------
  file { '/srv/nfs':
    ensure => directory,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
  }

  file { '/srv/nfs/wp-uploads':
    ensure  => directory,
    owner   => 'www-data',
    group   => 'www-data',
    mode    => '0775',
    require => File['/srv/nfs'],
  }

  # ---------------------------------------------------------------------------
  # NFS EXPORTS CONFIGURATION
  # ---------------------------------------------------------------------------
  file { '/etc/exports':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    # Map every client identity to www-data instead of trusting root over NFS.
    # The CMS only needs to write uploads, and anonuid/anongid 33 keeps the
    # files usable by Apache without granting root on the storage server.
    content => "/srv/nfs/wp-uploads ${nfs_clients}\n",
    require => [
      Package['nfs-kernel-server'],
      File['/srv/nfs/wp-uploads'],
    ],
    notify  => Exec['exportfs-reload'],
  }

  exec { 'exportfs-reload':
    command     => '/usr/sbin/exportfs -ra',
    refreshonly => true,
    require     => Package['nfs-kernel-server'],
  }

  service { 'nfs-kernel-server':
    ensure  => running,
    enable  => true,
    require => [
      Package['nfs-kernel-server'],
      File['/etc/exports'],
    ],
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for NFS
  # ---------------------------------------------------------------------------
  # cms_frontend pins nfsvers=4: only 2049/tcp is needed, not rpcbind or UDP.
  # Install all per-frontend rules before removing the old Main grants.
  $cms_frontend_ips.each |$ip| {
    exec { "ufw-storage-nfs-${ip}":
      command => "/usr/sbin/ufw allow from ${ip} to any port 2049 proto tcp comment \"NFSv4 CMS frontend ${ip}\"",
      unless  => "/usr/sbin/ufw status | /usr/bin/grep -E '^2049/tcp[[:space:]]+ALLOW[[:space:]]' | /usr/bin/grep -Fq ' ${ip} '",
      require => Exec['ufw-enable'],
    }
  }
  $nfs_firewall_rules = $cms_frontend_ips.map |$ip| { Exec["ufw-storage-nfs-${ip}"] }

  # Migrate both the original protocol-agnostic grants and the later TCP-only
  # Main grant; retaining either 2049 rule would still admit the hotdesks.
  $legacy_nfs_rules = { '2049' => '2049', '2049/tcp' => '2049 proto tcp', '111' => '111' }
  $legacy_nfs_rules.each |$port, $arguments| {
    exec { "ufw-storage-remove-legacy-${port}":
      command => "/usr/sbin/ufw --force delete allow from ${main_cidr} to any port ${arguments}",
      onlyif  => "/usr/sbin/ufw status | /usr/bin/grep -E '^${port}[[:space:]]+ALLOW[[:space:]]' | /usr/bin/grep -Fq ' ${main_cidr} '",
      require => $nfs_firewall_rules,
    }
  }
}
