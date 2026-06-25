# FASE 11 — Documentación Final



## Objetivos a conseguir

- [x] Listar inventario de IPs.
- [x] Crear manual de administración y recuperación de servicio.
- [x] Documentar Software Baseline (versiones + URLs) final.
- [x] Preparar tag del repo para evaluación física.

---

## Documentación Generada

### Inventario de IPs

Documentado en:
- [`docs/PLAN.md`](../PLAN.md) — Tabla completa de nodos con IP, MAC, rol, RAM, disco
- [`docs/phases/FASE-00.md`](FASE-00.md) — Diseño de red y direccionamiento

### Manual de Administración

Documentado en [`docs/MANUAL.md`](../MANUAL.md), que cubre:
1. Requisitos previos (hardware, software)
2. Despliegue completo paso a paso
3. Verificación del despliegue
4. Acceso a monitorización (Grafana, Prometheus)
5. Ampliar puestos hot-desk
6. Sustituir/ampliar frontales web
7. Sustituir/ampliar nodos de base de datos
8. Recuperación ante fallos (DRBD failover, K3s, LB)
9. Mantenimiento (backups, logs, certificados, actualizaciones)

### Software Baseline

Documentado en [`docs/SOFTWARE_BASELINE.md`](../SOFTWARE_BASELINE.md), que incluye:
- Nombre del software, versión, tipo, URL de origen y nodos donde se instala
- Cubre: Ubuntu, Cobbler, Puppet, Nginx, K3s, MariaDB, WordPress, Apache, PHP, Prometheus, Grafana, exportadores, DRBD, UFW, OpenSSL, libvirt/KVM

### Diagramas de Red

Documentados en [`docs/RED_DIAGRAMA.md`](../RED_DIAGRAMA.md):
- Diagrama de topología de red (Mermaid)
- Diagrama de arquitectura de servicios (Mermaid)
- Diagrama de secuencia de despliegue (Mermaid)

### Tag del Repositorio

```bash
# Crear tag para la entrega:
git add -A
git commit -m "Entrega final: infraestructura CMS completa"
git tag -a v1.0 -m "Entrega final GAR - Infraestructura CMS Fake Enterprise"
git push origin main --tags
```

### Estructura Final del Repositorio

```
TrabajoFinal/
├── README.md                    # Descripción general del proyecto
├── deploy_all.sh                # Orquestador principal
├── .gitignore                   # Exclusiones del repositorio
├── docs/
│   ├── PLAN.md                  # Plan de acción completo
│   ├── MANUAL.md                # Manual de administración
│   ├── SOFTWARE_BASELINE.md     # Inventario de software
│   ├── RED_DIAGRAMA.md          # Diagramas de red (Mermaid)
│   └── phases/FASE-00..11.md    # Documentación técnica por fase
├── scripts/
│   ├── 00_init_vms.sh           # Creación de VMs y redes
│   ├── 00_setup_cobbler.sh      # Cobbler (baremetal)
│   ├── 01_setup_puppet.sh       # Puppet (config management)
│   ├── 02_setup_nginx.sh        # Nginx + WordPress
│   ├── 03_setup_kubernetes.sh   # K3s + MariaDB
│   ├── 04_setup_monitoring.sh   # Prometheus + Grafana
│   ├── 05_setup_ufw.sh          # UFW (firewall)
│   ├── 06_setup_drbd.sh         # DRBD (HA)
│   ├── 07_traffic_mix.sh        # Generador de tráfico
│   └── add_cobbler_nodes.sh     # Registro de nodos en Cobbler
├── kubernetes/                  # Manifiestos K8s
│   ├── namespace.yaml
│   ├── mariadb-*.yaml
│   └── init-db-job.yaml
├── puppet/                      # Manifiestos Puppet
│   ├── manifests/site.pp
│   └── modules/role/manifests/
└── terraform/                   # Alternativa IaC declarativa (libvirt/KVM)
    ├── main.tf                  # Definición de recursos (VMs, redes, pools)
    ├── variables.tf             # Parámetros configurables (CPU, RAM, discos)
    ├── outputs.tf               # Salidas generadas tras el despliegue
    ├── versions.tf              # Restricciones de versión de proveedores
    └── README.md                # Guía de uso rápido de Terraform
```
