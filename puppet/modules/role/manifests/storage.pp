# role::storage — Centralised NFS storage server (internal-storage node).
#
# Manages: nfs-kernel-server, shared WordPress uploads directory,
# NFS exports for client subnets, and UFW firewall rules.

class role::storage {

  include role::base

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
    content => "/srv/nfs/wp-uploads 192.168.20.0/24(rw,sync,no_subtree_check,all_squash,anonuid=33,anongid=33)\n",
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
  exec { 'ufw-storage-nfs':
    command => '/usr/sbin/ufw allow from 192.168.20.0/24 to any port 2049 comment "NFS from Main subnet"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2049.*192.168.20.0/24"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-storage-rpcbind':
    command => '/usr/sbin/ufw allow from 192.168.20.0/24 to any port 111 comment "RPCbind from Main subnet"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "111.*192.168.20.0/24"',
    require => Exec['ufw-enable'],
  }
}
