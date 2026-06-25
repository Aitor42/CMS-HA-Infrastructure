# PHASE 05 — CMS Frontends and Load Balancer

## Objectives to Achieve

- [x] Configure and provision Load Balancer in the `main` network.
- [x] Configure 2 HTTP/S servers as CMS frontends.
- [x] Connect the Load Balancer to the CMS servers.

---

## Technical Implementation

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

- `scripts/02_setup_nginx.sh` — Installs Nginx, generates SSL certificate, configures LB, and installs WordPress on both frontends

### Verification

```bash
# Verify Nginx status
ssh root@192.168.20.100 "nginx -t && systemctl status nginx"

# Verify web frontends individually
curl -s http://192.168.20.101/ | grep -i wordpress
curl -s http://192.168.20.102/ | grep -i wordpress

# Verify complete end-to-end load balancing (HTTPS)
curl -sk https://192.168.20.100/
```
