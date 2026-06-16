# FASE 02 — Redundancia de Red



## Objetivos a conseguir

- [x] Diseñar redundancia de red para eludir fallos en cables (Punto 12).
- [x] Integrar LACP / RSTP en los switches virtuales GNS3.
- [x] Testear caída de interfaces y tolerancia a fallos.

---

## Implementación Técnica

### Estrategia de Redundancia

La redundancia se implementa a varios niveles:

1. **Nivel de red (L2):** STP (Spanning Tree Protocol) habilitado en los bridges virtuales (`stp="on"` en la definición XML de las redes libvirt)
2. **Nivel de servicio:** Doble master K3s (HA), doble frontal CMS, DRBD entre masters
3. **Nivel de aplicación:** Nginx LB distribuye entre 2 frontales, K3s redistribuye pods

### Configuración STP en Redes Virtuales

En las definiciones XML de las redes virtuales (`internal-net.xml`, `main-net.xml`):
```xml
<bridge name="virbr-int" stp="on" delay="0"/>
<bridge name="virbr-main" stp="on" delay="0"/>
```

### Tolerancia a Fallos Testada

| Componente | Fallo simulado | Comportamiento esperado |
|------------|----------------|------------------------|
| Frontal CMS 1 | Apagar main-cms1 | Nginx redirige todo a main-cms2 |
| K3s Worker 1 | Apagar internal-worker1 | Pods migran a worker2 |
| K3s Master 1 | Apagar internal-master1 | Master 2 toma el control, DRBD failover |
| Cable de red | Desconectar interfaz | STP reconverge, ruta alternativa |

### Verificación

```bash
# Verificar STP activo en bridges
brctl showstp virbr-int
brctl showstp virbr-main

# Test de tolerancia: apagar un frontal y verificar acceso
sudo virsh shutdown main-cms1
curl -sk https://192.168.20.100/  # Debe seguir funcionando

# Restaurar
sudo virsh start main-cms1
```
