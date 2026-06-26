# role::k3s_worker — K3s worker node (internal-worker1, internal-worker2).
#
# Manages: base dependencies and UFW rules for K3s worker nodes.
# K3s agent join token logic stays in 06_setup_kubernetes.sh.

class role::k3s_worker {

  include role::base

  package { ['curl', 'apt-transport-https', 'ca-certificates']:
    ensure => installed,
  }

  # Kubelet API
  exec { 'ufw-kubelet-worker':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 10250 proto tcp comment "Kubelet API"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "10250/tcp"',
    require => Exec['ufw-enable'],
  }

  # Flannel VXLAN overlay
  exec { 'ufw-flannel-worker':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 8472 proto udp comment "Flannel VXLAN"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "8472/udp"',
    require => Exec['ufw-enable'],
  }
}
