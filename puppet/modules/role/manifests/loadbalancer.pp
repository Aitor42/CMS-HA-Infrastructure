# role::loadbalancer — Nginx reverse proxy and SSL load balancer (main-lb).
#
# Manages: Nginx package, upstream config, self-signed SSL certificate,
# and UFW rules for HTTP/HTTPS/metrics access.

class role::loadbalancer {

  include role::base

  # ---------------------------------------------------------------------------
  # PACKAGES
  # ---------------------------------------------------------------------------
  package { ['nginx', 'openssl']:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # SSL — Self-signed certificate (generated once, never overwritten)
  # ---------------------------------------------------------------------------
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

  service { 'nginx':
    ensure  => running,
    enable  => true,
    require => Package['nginx'],
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for load balancer
  # ---------------------------------------------------------------------------
  exec { 'ufw-lb-http':
    command => '/usr/sbin/ufw allow 80/tcp comment "HTTP"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "80/tcp.*ALLOW IN"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-lb-https':
    command => '/usr/sbin/ufw allow 443/tcp comment "HTTPS"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "443/tcp.*ALLOW IN"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-lb-nginx-exporter':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 9113 proto tcp comment "nginx_exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9113"',
    require => Exec['ufw-enable'],
  }
}
