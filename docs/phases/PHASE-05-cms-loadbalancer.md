# PHASE 05 — CMS Frontends and Load Balancer

## Objectives to Achieve

- [x] Configure and provision a **redundant** Load Balancer pair (main-lb1/main-lb2) sharing a keepalived VIP in the `main` network.
- [x] Configure 2 HTTP/S servers as CMS frontends.
- [x] Connect the Load Balancer to the CMS servers.
- [x] Configure shared persistence for WordPress uploads via NFS.

---

## Technical Implementation

### Shared Persistence — NFS Storage (internal-storage: 192.168.10.15)

- **Software:** `nfs-kernel-server` (storage node), `nfs-common` (CMS frontend nodes)
- **Export directory:** `/srv/nfs/wp-uploads` (owner: `www-data:www-data`, mode: `0775`)
- **Mount target on CMS frontends:** `/var/www/html/wp-content/uploads/`
- **Mount options:** `_netdev,auto,rw,hard,timeo=600,retrans=2,nfsvers=4` (no `soft` or `nofail`).
- **Export options:** `rw,sync,no_subtree_check,all_squash,anonuid=33,anongid=33` — both root and `www-data` writers map to uid/gid 33. Export clients and NFSv4 `2049/tcp` UFW grants are limited to the literal frontend IPv4 addresses generated as `cms_ha::cms_frontend_ips`; empty/invalid allowlists fail the storage catalog. Legacy Main-subnet NFS/RPC grants are removed.
- **Network routing:** Routed across subnets from `192.168.20.0/24` to `192.168.10.15:2049` via `ufw-router`.
- **Startup safety:** Apache requires and binds to the uploads mount, with a pre-start check for the exact writable hard NFS export and filesystem root. When unmounted, the underlying local directory is root-owned `0555`. Bootstrap uses a shared NFS `flock` and rechecks WordPress installation inside the lock.
- **Availability limit:** Both frontends share the uploads, but the single NFS server is not redundant. A hard mount can block I/O during a storage outage; it does not guarantee continuous CMS availability. Existing soft mounts require controlled migration with Apache stopped; see [MANUAL section 6.2](../MANUAL.md#62-wordpress-media-and-file-backups).

### Nginx Load Balancers + keepalived VIP (main-lb1: .103, main-lb2: .104, VIP: 192.168.20.100)

- **Software:** Nginx 1.24.x + keepalived
- **High availability:** Both balancers serve the same upstream pool; keepalived
  holds a **floating VIP (192.168.20.100)** using VRRP. `main-lb1` is the
  preferred MASTER (priority 101), `main-lb2` the BACKUP (priority 100). VRRP
  runs **unicast** between the two balancers' main-network addresses, and a
  `track_script` demotes a node whose Nginx is unhealthy so the VIP always
  follows a working balancer.
- **Clients and the router DNAT** target the VIP, never the individual nodes.
- **TLS:** Certificate issued by the internal **step-ca** CA (`/etc/nginx/ssl/cms.crt`),
  with SANs for `cms.fake-enterprise.com`, the VIP (`192.168.20.100`) and both
  balancer addresses. It is issued once by the PKI phase on the CA host and
  copied to both balancers (no CA credential is stored on them). A self-signed
  certificate is generated only as a bootstrap fallback so Nginx can start
  before the CA exists; it carries the same vhost/VIP/balancer SANs, so the
  nginx phase can probe HTTPS before the PKI phase replaces it.
- **Automatic renewal:** `role::loadbalancer` deploys `cms-tls-renew.sh` plus a
  daily systemd timer (`cms-tls-renew.timer`). Renewal uses `step ca renew`
  (mTLS, no secret) and reloads Nginx when the certificate is replaced, keeping
  its remaining validity comfortably above 30 days. The PKI phase writes the CA
  URL, root and name it actually provisioned to `/etc/cms-ha/pki.env`; the
  renewal script sources that file instead of keeping a second hardcoded copy.
- **Port 80:** Redirects to HTTPS (301)
- **Port 443:** Reverse proxy with TLS to the frontend pool
- **Firewall:** Both nodes allow IP protocol 112 (VRRP) from `192.168.20.0/24`; the CA
  host allows TCP 8443 (step-ca) from both lab subnets.

**Upstream configuration:**
```nginx
upstream cms_backend {
    server 192.168.20.101;
    server 192.168.20.102;
}

server {
    listen 80;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    ssl_certificate /etc/nginx/ssl/cms.crt;
    ssl_certificate_key /etc/nginx/ssl/cms.key;

    client_max_body_size 32m;

    location / {
        proxy_pass http://cms_backend;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### WordPress Frontends (main-cms1/2)

- **Software:** Apache 2.4.x + PHP 8.3.x + WordPress 6.x
- **Web root directory:** `/var/www/html/`
- **Database connection:** MariaDB NodePort endpoints on the first two configured masters (`192.168.10.11:30306`, then `192.168.10.12:30306` by default), probed in order by the generated WordPress configuration.
- **wp-config.php settings:**
  - DB_NAME: `wordpress`
  - DB_USER: `wp_user`
  - DB_HOST: selected reachable NodePort endpoint from the generated host list
- **PHP runtime:** Puppet and verification enforce a package floor of
  `8.3.6-0ubuntu0.24.04.13` for CLI, common and mod_php packages, with
  `session.upload_progress.enabled=Off`. Apache enforces a non-overridable
  `php_admin_value memory_limit 128M`; the standard prefork configuration sets
  both `ServerLimit 4` and `MaxRequestWorkers 4` (`StartServers 2`, spare workers
  1–2, `MaxConnectionsPerChild 500`). Verification checks these enabled configs.
  Request bodies/posts are capped at 32 MiB and uploads at 16 MiB. Body limits
  do not bound all allocations: four PHP budgets total 512 MiB before native,
  Apache and OS overhead, so this is not a proof that OOM is impossible on a
  1 GiB frontend. Finite limits do not replace patched packages.
  Details in `docs/SECURITY.md` § 4.

### Implementation

- `internal/phases/nginx` (`cms-ha phase setup-nginx-wordpress`) — Nginx, keepalived and the WordPress frontends.
- `internal/phases/pki` (`cms-ha phase setup-ca`) — issues the balancer certificates from the internal CA.
- `puppet/modules/role` (`role::loadbalancer`, `role::cms_frontend`) — desired state of the balancers and frontends.

### Verification

```bash
# Verify Nginx status on both balancers
ssh ubuntu@192.168.20.103 "sudo -H nginx -t && systemctl is-active nginx && systemctl is-active keepalived"
ssh ubuntu@192.168.20.104 "sudo -H nginx -t && systemctl is-active nginx && systemctl is-active keepalived"

# Verify exactly one balancer owns the VIP (the nginx phase fails otherwise)
for ip in 192.168.20.103 192.168.20.104; do
  echo -n "$ip: "
  ssh ubuntu@$ip "ip -4 -o addr show | grep -w 192.168.20.100 || echo not-holder"
done

# Verify each balancer answers the configured vhost directly. TLS terminates
# here and the phase runs before the PKI phase, so the bootstrap certificate is
# skipped with -k; `cms-ha verify` later checks the CA-issued certificate.
for ip in 192.168.20.103 192.168.20.104; do
  ssh ubuntu@$ip "curl -s -k -f --resolve cms.fake-enterprise.com:443:127.0.0.1 -o /dev/null -w '%{http_code}\n' https://cms.fake-enterprise.com/"
done

# Verify web frontends individually. TLS terminates on the LBs, so the vhost
# answers a plain HTTP request with a redirect to HTTPS (expect 301).
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: cms.fake-enterprise.com' http://192.168.20.101/
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: cms.fake-enterprise.com' http://192.168.20.102/

# Verify complete end-to-end load balancing (HTTPS via the VIP)
curl -sk https://192.168.20.100/

# Failover drill: stop keepalived/nginx on the MASTER and confirm the VIP moves
ssh ubuntu@192.168.20.103 "sudo systemctl stop keepalived"
ssh ubuntu@192.168.20.104 "ip -4 addr show | grep 192.168.20.100"   # VIP now here
curl -sk https://192.168.20.100/                                        # still served
ssh ubuntu@192.168.20.103 "sudo systemctl start keepalived"

# Verify the NFS export carries the identity mapping both writers rely on
ssh ubuntu@192.168.10.15 "sudo -H exportfs -s | grep /srv/nfs/wp-uploads | grep -E 'all_squash.*anonuid=33.*anongid=33'"

# Inspect the exact live mount, not just an ancestor filesystem or fstab
ssh ubuntu@192.168.20.101 "findmnt -rn -M /var/www/html/wp-content/uploads -o SOURCE,FSTYPE,OPTIONS,FSROOT"
ssh ubuntu@192.168.20.102 "sudo /usr/local/sbin/cms-ha-check-uploads 192.168.10.15:/srv/nfs/wp-uploads"
# On storage, inspect per-frontend export selectors and 2049/tcp grants;
# no Main-subnet-wide NFS/RPC grants should remain.
ssh ubuntu@192.168.10.15 "sudo exportfs -s; sudo ufw status"

# Verify the PHP runtime carries the fix for CVE-2026-109618 / CVE-2026-109619.
# Ubuntu backports the upstream patch, so the Debian revision is what tells a
# patched node from a vulnerable one (php -v keeps reporting 8.3.6). Check the
# Apache SAPI too: a patched CLI says nothing about the library mod_php loads.
ssh ubuntu@192.168.20.101 "dpkg-query -W -f='\${Package} \${Version}\n' php8.3-cli php8.3-common libapache2-mod-php8.3"
ssh ubuntu@192.168.20.102 "dpkg-query -W -f='\${Package} \${Version}\n' php8.3-cli php8.3-common libapache2-mod-php8.3"

# Verify the mitigations the PHP team publishes for those CVEs are in place
ssh ubuntu@192.168.20.101 "grep -E 'session.upload_progress.enabled' /etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini"
ssh ubuntu@192.168.20.101 "grep -E 'php_admin_value memory_limit' /etc/apache2/conf-enabled/php-hardening.conf"
ssh ubuntu@192.168.20.101 "grep -E 'ServerLimit|MaxRequestWorkers' /etc/apache2/mods-enabled/mpm_prefork.conf"
# Repeat on cms2; cms-ha verify checks both frontends.
```
