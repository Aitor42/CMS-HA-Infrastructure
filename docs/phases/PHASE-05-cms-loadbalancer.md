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
- **Mount options:** `_netdev,auto,nofail,rw,soft,timeo=50,retrans=3`
- **Network routing:** Routed across subnets from `192.168.20.0/24` to `192.168.10.15:2049` via `ufw-router`.
- **Result:** 100% HA symmetry in the web tier. Uploads written by either CMS node are immediately accessible to both nodes without data drift.

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
  before the CA exists.
- **Automatic renewal:** `role::loadbalancer` deploys `cms-tls-renew.sh` plus a
  daily systemd timer (`cms-tls-renew.timer`). Renewal uses `step ca renew`
  (mTLS, no secret) and reloads Nginx when the certificate is replaced, keeping
  its remaining validity comfortably above 30 days.
- **Port 80:** Redirects to HTTPS (301)
- **Port 443:** Reverse proxy with TLS to the frontend pool
- **Firewall:** Both nodes allow TCP 112 (VRRP) from `192.168.20.0/24`; the CA
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
- **Database connection:** `192.168.10.11:30306` (MariaDB NodePort in K3s)
- **wp-config.php settings:**
  - DB_NAME: `wordpress`
  - DB_USER: `wp_user`
  - DB_HOST: `192.168.10.11:30306`
- **PHP runtime:** the PHP 8.3 branch is pinned to the Ubuntu revision that
  carries the fix for CVE-2026-109618 / CVE-2026-109619 (*"php2shell"*), and
  the web server carries the mitigations the PHP team publishes for those
  bugs. The request size is bounded at every layer — `client_max_body_size`
  (nginx), `LimitRequestBody` (Apache) and `post_max_size` (PHP) — so a
  single request can never ask PHP for more memory than the node has.
  Details in `docs/SECURITY.md` § 4.

### Implementation

- `internal/phases/nginx` (`cms-ha phase setup-nginx-wordpress`) — Nginx, keepalived and the WordPress frontends.
- `internal/phases/pki` (`cms-ha phase setup-ca`) — issues the balancer certificates from the internal CA.
- `puppet/modules/role` (`role::loadbalancer`, `role::cms`) — desired state of the balancers and frontends.

### Verification

```bash
# Verify Nginx status on both balancers
ssh ubuntu@192.168.20.103 "sudo -H nginx -t && systemctl is-active nginx && systemctl is-active keepalived"
ssh ubuntu@192.168.20.104 "sudo -H nginx -t && systemctl is-active nginx && systemctl is-active keepalived"

# Verify the VIP is owned by the MASTER (main-lb1)
ssh ubuntu@192.168.20.103 "ip -4 addr show | grep 192.168.20.100"

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

# Verify NFS export on internal-storage
ssh ubuntu@192.168.10.15 "sudo -H exportfs -v && systemctl is-active nfs-kernel-server"

# Verify shared uploads mount on CMS frontends
ssh ubuntu@192.168.20.101 "mountpoint -q /var/www/html/wp-content/uploads && echo 'CMS1 NFS mounted'"
ssh ubuntu@192.168.20.102 "mountpoint -q /var/www/html/wp-content/uploads && echo 'CMS2 NFS mounted'"

# Verify the PHP runtime carries the fix for CVE-2026-109618 / CVE-2026-109619.
# Ubuntu backports the upstream patch, so the Debian revision is what tells a
# patched node from a vulnerable one (php -v keeps reporting 8.3.6).
ssh ubuntu@192.168.20.101 "dpkg-query -W -f='\${Version}\n' php8.3-cli"
ssh ubuntu@192.168.20.102 "dpkg-query -W -f='\${Version}\n' php8.3-cli"

# Verify the mitigations the PHP team publishes for those CVEs are in place
ssh ubuntu@192.168.20.101 "grep -E 'session.upload_progress.enabled' /etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini"
ssh ubuntu@192.168.20.101 "grep -E 'php_admin_value memory_limit' /etc/apache2/conf-enabled/php-hardening.conf"
```
