# FASE 05 — Frontales CMS y Balanceador



## Objetivos a conseguir

- [x] Configurar y provisionar Balanceador en `main` (Punto 4a).
- [x] Configurar 2 servidores HTTP/S como frontales del CMS (Punto 4b).
- [x] Conectar Load Balancer a los servidores CMS.

---

## Implementación Técnica

### Balanceador Nginx (main-lb: 192.168.20.100)

- **Software:** Nginx 1.24.x
- **SSL:** Certificado auto-firmado (`/etc/ssl/certs/cms-selfsigned.crt`)
- **Puerto 80:** Redirige a HTTPS (301)
- **Puerto 443:** Proxy inverso con SSL hacia los frontales

**Configuración del upstream:**
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

### Frontales WordPress (main-cms1/2)

- **Software:** Apache 2.4.x + PHP 8.3.x + WordPress 6.x
- **Documentos web:** `/var/www/html/`
- **Conexión a BD:** `192.168.10.11:30306` (NodePort de MariaDB en K3s)
- **Configuración wp-config.php:**
  - DB_NAME: `wordpress`
  - DB_USER: `wp_user`
  - DB_HOST: `192.168.10.11:30306`

### Scripts Asociados

- `scripts/02_setup_nginx.sh` — Instala Nginx, genera SSL, configura LB e instala WordPress en ambos frontales

### Verificación

```bash
# Verificar Nginx
ssh root@192.168.20.100 "nginx -t && systemctl status nginx"

# Verificar frontales
curl -s http://192.168.20.101/ | grep -i wordpress
curl -s http://192.168.20.102/ | grep -i wordpress

# Verificar balanceo completo (HTTPS)
curl -sk https://192.168.20.100/
```
