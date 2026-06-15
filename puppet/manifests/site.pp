# site.pp — Puppet node classifier.
#
# Maps each hostname pattern to its role class. Roles encapsulate all
# packages, config files, services and firewall rules for that node type.
# The Puppet agent runs every 30 minutes and enforces this desired state.

# Perimeter router and firewall
node /^ufw-router/ {
  include role::router
}

# Observability stack (Prometheus + Grafana)
node /^internal-monitor/ {
  include role::monitor
}

# K3s control-plane masters (also run DRBD for MariaDB HA)
node /^internal-master/ {
  include role::k3s_master
}

# K3s worker nodes
node /^internal-worker/ {
  include role::k3s_worker
}

# Nginx load balancer
node /^main-lb/ {
  include role::loadbalancer
}

# WordPress + Apache CMS frontends
node /^main-cms/ {
  include role::cms_frontend
}

# Hot-desk workstations (DHCP-provisioned, base config only)
node /^main-hotdesk/ {
  include role::hotdesk
}

# Storage and Jumpstart: base config only (services managed by other means)
node /^internal-storage/ {
  include role::base
}

node /^jumpstart/ {
  include role::base
}

# Catch-all for any unclassified node
node default {
  include role::base
}
