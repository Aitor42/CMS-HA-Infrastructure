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
    content => "/srv/nfs/wp-uploads 192.168.20.0/24(rw,sync,no_subtree_check,no_root_squash)\n",
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
    command => '/usr/sbin/ufw allow from 192.168.20.0/24 to any port 2049 proto tcp comment "NFS from Main subnet"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2049/tcp.*192.168.20.0/24"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-storage-rpcbind':
    command => '/usr/sbin/ufw allow from 192.168.20.0/24 to any port 111 proto tcp comment "RPCbind from Main subnet"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "111/tcp.*192.168.20.0/24"',
    require => Exec['ufw-enable'],
  }
}
