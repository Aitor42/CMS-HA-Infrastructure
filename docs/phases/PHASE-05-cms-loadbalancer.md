# PHASE 05 — CMS Frontends and Load Balancer

## Objectives to Achieve

- [x] Configure and provision Load Balancer in the `main` network.
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

### Nginx Load Balancer (main-lb: 192.168.20.100)

- **Software:** Nginx 1.24.x
- **SSL:** Self-signed certificate (`/etc/ssl/certs/cms-selfsigned.crt`)
- **Port 80:** Redirects to HTTPS (301)
- **Port 443:** Reverse proxy with SSL to the frontend pool

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
    ssl_certificate /etc/ssl/certs/cms-selfsigned.crt;
    ssl_certificate_key /etc/ssl/private/cms-selfsigned.key;

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

### Associated Scripts

- `scripts/07_setup_nginx_wordpress.sh` — Installs Nginx, generates SSL certificate, configures LB, and installs WordPress on both frontends

### Verification

```bash
# Verify Nginx status
ssh root@192.168.20.100 "nginx -t && systemctl status nginx"

# Verify web frontends individually
curl -s http://192.168.20.101/ | grep -i wordpress
curl -s http://192.168.20.102/ | grep -i wordpress

# Verify complete end-to-end load balancing (HTTPS)
curl -sk https://192.168.20.100/

# Verify NFS export on internal-storage
ssh root@192.168.10.15 "exportfs -v && systemctl is-active nfs-kernel-server"

# Verify shared uploads mount on CMS frontends
ssh root@192.168.20.101 "mountpoint -q /var/www/html/wp-content/uploads && echo 'CMS1 NFS mounted'"
ssh root@192.168.20.102 "mountpoint -q /var/www/html/wp-content/uploads && echo 'CMS2 NFS mounted'"
```
