# role::loadbalancer — Nginx reverse proxy + keepalived VIP (main-lb1/main-lb2).
#
# Manages: Nginx package, upstream config, TLS certificate (issued by the
# internal step-ca) + automated renewal timer, keepalived (shared VIP
# 192.168.20.100), and UFW rules for HTTP/HTTPS/metrics/VRRP.

class role::loadbalancer {

  include role::base

  # ---------------------------------------------------------------------------
  # PACKAGES
  # ---------------------------------------------------------------------------
  package { ['nginx', 'openssl', 'prometheus-nginx-exporter', 'keepalived', 'wget']:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # SSL/TLS — certificate issued by the internal CA (step-ca)
  # ---------------------------------------------------------------------------
  # The real certificate is issued by the Go PKI phase on the CA host and
  # copied here (so no CA provisioner credential is stored on the balancers).
  # This role owns the tooling and the renewal timer. A self-signed certificate
  # is generated once as a bootstrap fallback so nginx can start before the CA
  # exists; the PKI phase overwrites it with the CA-issued certificate.
  file { '/etc/nginx/ssl':
    ensure  => directory,
    owner   => 'root',
    group   => 'root',
    mode    => '0700',
    require => Package['nginx'],
  }

  exec { 'generate-ssl-cert':
    command => '/usr/bin/openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
      -keyout /etc/nginx/ssl/cms.key \
      -out    /etc/nginx/ssl/cms.crt \
      -subj "/C=ES/ST=PaisVasco/L=Bilbao/O=FakeEnterprise/OU=IT/CN=cms.fake-enterprise.com"',
    creates => '/etc/nginx/ssl/cms.crt',
    require => File['/etc/nginx/ssl'],
  }

  # step CLI is required to renew the CA-issued certificate. Pinned to the same
  # release as pki.step_version in config.yaml (keep the two in sync): the
  # version-independent "latest" alias would silently change what a fresh
  # balancer installs. `set -e` plus the trailing `test -x` matter because the
  # previous one-liner ended in `; rm -f`, so a failed download (or a missing
  # wget) exited 0: Puppet saw success, `creates` never became true and the exec
  # silently re-ran on every convergence without ever installing step.
  exec { 'install-step-cli':
    command => '/bin/bash -c "set -e; wget -q https://github.com/smallstep/cli/releases/download/v0.31.0/step-cli_0.31.0-1_$(dpkg --print-architecture).deb -O /tmp/step-cli.deb; \
      dpkg -i /tmp/step-cli.deb || apt-get install -f -y; rm -f /tmp/step-cli.deb; test -x /usr/bin/step"',
    creates => '/usr/bin/step',
    path    => ['/usr/bin', '/bin', '/usr/sbin', '/sbin'],
    timeout => 300,
    require => Package['nginx'],
  }

  file { '/usr/local/bin/cms-tls-renew.sh':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0755',
    source  => 'puppet:///modules/role/nginx/cms-tls-renew.sh',
    require => Exec['install-step-cli'],
  }

  file { '/etc/systemd/system/cms-tls-renew.service':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0644',
    source => 'puppet:///modules/role/nginx/cms-tls-renew.service',
    notify => Exec['cms-tls-renew-daemon-reload'],
  }

  file { '/etc/systemd/system/cms-tls-renew.timer':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0644',
    source => 'puppet:///modules/role/nginx/cms-tls-renew.timer',
    notify => Exec['cms-tls-renew-daemon-reload'],
  }

  exec { 'cms-tls-renew-daemon-reload':
    command     => '/bin/systemctl daemon-reload',
    refreshonly => true,
    before      => Service['cms-tls-renew.timer'],
  }

  service { 'cms-tls-renew.timer':
    ensure  => running,
    enable  => true,
    require => [
      File['/usr/local/bin/cms-tls-renew.sh'],
      File['/etc/systemd/system/cms-tls-renew.service'],
      File['/etc/systemd/system/cms-tls-renew.timer'],
      Exec['cms-tls-renew-daemon-reload'],
    ],
  }

  # ---------------------------------------------------------------------------
  # NGINX CONFIG — Upstream pool + SSL reverse proxy
  # ---------------------------------------------------------------------------

  # Disable default site
  file { '/etc/nginx/sites-enabled/default':
    ensure  => absent,
    require => Package['nginx'],
    notify  => Service['nginx'],
  }

  # Deploy the load-balancer config from the Puppet module fileserver
  file { '/etc/nginx/conf.d/cms_lb.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    source  => 'puppet:///modules/role/nginx/cms-lb.conf',
    require => [Package['nginx'], Exec['generate-ssl-cert']],
    notify  => Service['nginx'],
  }

  # Validate config before reloading (prevents broken reloads)
  exec { 'nginx-config-test':
    command     => '/usr/sbin/nginx -t',
    refreshonly => true,
    subscribe   => File['/etc/nginx/conf.d/cms_lb.conf'],
    before      => Service['nginx'],
  }

  file { '/etc/nginx/conf.d/stub_status.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "server {\n    listen 127.0.0.1:8080;\n    location /stub_status {\n        stub_status on;\n        allow 127.0.0.1;\n        deny all;\n    }\n    location /nginx_status {\n        stub_status on;\n        allow 127.0.0.1;\n        deny all;\n    }\n}\n",
    require => Package['nginx'],
    notify  => Service['nginx'],
  }

  service { 'nginx':
    ensure  => running,
    enable  => true,
    require => Package['nginx'],
  }

  service { 'prometheus-nginx-exporter':
    ensure  => running,
    enable  => true,
    require => [
      Package['prometheus-nginx-exporter'],
      Service['nginx'],
      File['/etc/nginx/conf.d/stub_status.conf'],
    ],
  }

  # ---------------------------------------------------------------------------
  # KEEPALIVED — Floating VIP (192.168.20.100) shared by both LBs
  # ---------------------------------------------------------------------------
  # main-lb1 is the preferred MASTER, main-lb2 the BACKUP. VRRP runs unicast
  # between the two nodes over the main network; a track_script demotes a node
  # whose nginx is unhealthy so the VIP follows a working balancer.
  $lb_is_master = $facts['networking']['hostname'] == 'main-lb1'
  $lb_peer  = $lb_is_master ? { true => '192.168.20.104', default => '192.168.20.103' }
  $lb_prio  = $lb_is_master ? { true => '101',           default => '100' }
  $lb_state = $lb_is_master ? { true => 'MASTER',        default => 'BACKUP' }

  # VRRP runs over the main network, but the kernel name of that NIC must not
  # be hardcoded: a load balancer only has one interface (enp1s0), while a node
  # with more NICs names the second one enp2s0. Resolve it from the primary
  # interface, which is the one carrying this node's address and default route,
  # and fail loudly instead of compiling a catalog with an undefined address.
  $lb_primary = $facts['networking']['primary']
  $lb_iface_candidates = $facts['networking']['interfaces'].filter |$name, $data| {
    $name == $lb_primary and $data['ip'] != undef
  }
  $lb_ifaces = $lb_iface_candidates.keys
  if $lb_ifaces.length == 0 {
    fail("role::loadbalancer: no IPv4 address on the primary interface (${lb_primary}) in facts; cannot configure keepalived")
  }
  $lb_iface  = $lb_ifaces[0]
  $lb_src_ip = $lb_iface_candidates[$lb_iface]['ip']

  file { '/etc/keepalived/keepalived.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => @("KEEPALIVED"),
      # Managed by Puppet (role::loadbalancer). Do not edit by hand.
      vrrp_script chk_nginx {
          script "/usr/bin/systemctl is-active --quiet nginx"
          interval 2
          weight -20
          fall 2
          rise 2
      }

      vrrp_instance CMS_VIP {
          state ${lb_state}
          interface ${lb_iface}
          virtual_router_id 51
          priority ${lb_prio}
          advert_int 1
          unicast_src_ip ${lb_src_ip}
          unicast_peer {
              ${lb_peer}
          }
          authentication {
              auth_type PASS
              auth_pass cmshakeep
          }
          virtual_ipaddress {
              192.168.20.100/24
          }
          track_script {
              chk_nginx
          }
      }
      | KEEPALIVED
    require => Package['keepalived'],
    notify  => Service['keepalived'],
  }

  service { 'keepalived':
    ensure  => running,
    enable  => true,
    require => Package['keepalived'],
  }

  # ---------------------------------------------------------------------------
  # KEEPALIVED METRICS — VIP ownership published to node_exporter (textfile)
  # ---------------------------------------------------------------------------
  # Exactly one balancer must hold the VIP. cms_keepalived_state lets Prometheus
  # alert on "nobody is MASTER" (0) or a VRRP split (>1).
  file { '/usr/local/bin/cms-keepalived-metrics.sh':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
    source => 'puppet:///modules/role/cms-ha/cms-keepalived-metrics.sh',
  }

  cron { 'cms-keepalived-metrics':
    ensure  => present,
    command => '/usr/local/bin/cms-keepalived-metrics.sh 192.168.20.100 >/dev/null 2>&1',
    user    => 'root',
    minute  => '*',
    require => [
      File['/usr/local/bin/cms-keepalived-metrics.sh'],
      File['/var/lib/prometheus/node-exporter'],
      Service['keepalived'],
    ],
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for load balancer
  # ---------------------------------------------------------------------------
  exec { 'ufw-lb-http':
    command => '/usr/sbin/ufw allow 80/tcp comment "HTTP"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "80/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-lb-https':
    command => '/usr/sbin/ufw allow 443/tcp comment "HTTPS"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "443/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-lb-nginx-exporter':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 9113 proto tcp comment "nginx_exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9113"',
    require => Exec['ufw-enable'],
  }

  # Keepalived speaks VRRP (IP protocol 112) between the two balancers. ufw
  # only accepts protocol *names* from its supported list, so "proto 112" is
  # rejected ("ERROR: Unsupported protocol '112'") and the catalog fails; the
  # supported spelling renders in `ufw status` as "192.168.20.0/24/vrrp".
  exec { 'ufw-lb-vrrp':
    command => '/usr/sbin/ufw allow from 192.168.20.0/24 proto vrrp comment "keepalived VRRP"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "vrrp"',
    require => Exec['ufw-enable'],
  }
}
