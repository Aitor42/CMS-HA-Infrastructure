# FASE 10 — TrafficMix, Tests End-to-End y Jumpstart OFF



## Objetivos a conseguir

- [x] Probar generación de tráfico con TrafficMix desde WAN (Punto 15).
- [x] Probar scripts o consultas DB desde los hot-desks integrados.
- [x] Apagar nodo Jumpstart y demostrar pervivencia del sistema completo (Punto 14).

---

## Implementación Técnica

### Script de TrafficMix

El script `scripts/07_traffic_mix.sh` genera tráfico variado contra la infraestructura CMS. Soporta dos modos:

**Modo externo** (simula tráfico desde internet):
```bash
./scripts/07_traffic_mix.sh --external --target <IP_WAN_ROUTER> --duration 120
```

**Modo interno** (desde un hot-desk):
```bash
./scripts/07_traffic_mix.sh --internal --duration 60 --with-db
```

### Tipos de Tráfico Generado

| Tipo | Herramienta | Descripción |
|------|-------------|-------------|
| Páginas WordPress | curl | GET a /, /wp-login.php, /?s=query, /feed/, API REST |
| Carga concurrente | ab | N peticiones concurrentes al LB |
| Formularios POST | curl | Intentos de login, búsquedas por POST |
| Consultas BD | mysql | SELECT, SHOW TABLES directamente a MariaDB |

### Test de Apagado del Jumpstart (Punto 14)

```bash
# 1. Verificar que todo funciona
curl -sk https://192.168.20.100/
ssh root@192.168.10.11 "kubectl get nodes"

# 2. Apagar el jumpstart
sudo virsh shutdown jumpstart

# 3. Verificar que todo SIGUE funcionando
curl -sk https://192.168.20.100/           # CMS accesible
ssh root@192.168.10.11 "kubectl get nodes"  # K3s operativo
ssh root@192.168.10.11 "kubectl get pods -n cms"  # MariaDB running

# 4. Generar tráfico post-apagado
./scripts/07_traffic_mix.sh --internal --duration 30
```

### Resultados Esperados

- El CMS sigue respondiendo normalmente tras apagar jumpstart
- Las métricas de Prometheus/Grafana siguen recolectándose
- El clúster K3s sigue operativo con sus 4 nodos
- Solo se pierde la capacidad de provisionar nuevos nodos

### Verificación

```bash
# Resultado del TrafficMix (informe final)
./scripts/07_traffic_mix.sh --internal --duration 60

# El script muestra:
# - Total de peticiones
# - Peticiones exitosas vs fallidas
# - Tasa de peticiones/segundo (si se usa ab)
```
