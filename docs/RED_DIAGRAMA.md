# Network Architecture Diagrams — CMS Infrastructure

This document contains the technical architecture diagrams for the CMS high-availability infrastructure, rendered with Mermaid.

---

## 1. Complete Network Topology

Diagram showing the physical and logical network segmentation, including all nodes, their IP addresses, and inter-network connections.

```mermaid
graph TB
    subgraph INTERNET["☁ Internet / WAN"]
        CLOUD["External Clients"]
    end

    subgraph FW["Router / Firewall — ufw-router"]
        ETH0["eth0<br/>DHCP (WAN)"]
        ETH1["eth1<br/>192.168.10.1"]
        ETH2["eth2<br/>192.168.20.1"]
        UFW_CORE["UFW + NAT<br/>IP Forwarding"]
        ETH0 --- UFW_CORE
        UFW_CORE --- ETH1
        UFW_CORE --- ETH2
    end

    subgraph INTERNAL["Internal Network — 192.168.10.0/24"]
        direction TB
        JS["🖥 jumpstart<br/>192.168.10.10<br/>Cobbler + Puppet Server"]

        subgraph K3S_CLUSTER["K3s HA Cluster"]
            M1["🟢 internal-master1<br/>192.168.10.11<br/>K3s Server + DRBD"]
            M2["🟢 internal-master2<br/>192.168.10.12<br/>K3s Server + DRBD"]
            W1["🔵 internal-worker1<br/>192.168.10.13<br/>K3s Agent"]
            W2["🔵 internal-worker2<br/>192.168.10.14<br/>K3s Agent"]
        end

        ST["💾 internal-storage<br/>192.168.10.15<br/>Centralised Storage"]
        MON["📊 internal-monitor<br/>192.168.10.20<br/>Prometheus + Grafana"]
    end

    subgraph MAIN["Client Network — 192.168.20.0/24"]
        direction TB
        LB["⚖ main-lb<br/>192.168.20.100<br/>Nginx LB"]

        subgraph CMS_POOL["CMS Frontends"]
            CMS1["🌐 main-cms1<br/>192.168.20.101<br/>WordPress + Apache"]
            CMS2["🌐 main-cms2<br/>192.168.20.102<br/>WordPress + Apache"]
        end

        subgraph HOTDESKS["Hot-desk Workstations"]
            HD1["💻 main-hotdesk1<br/>192.168.20.201"]
            HD2["💻 main-hotdesk2<br/>192.168.20.202"]
            HD38["💻 main-hotdesk3..8<br/>192.168.20.203-208"]
        end
    end

    CLOUD -->|"HTTP/S ports 80/443"| ETH0
    ETH1 --> INTERNAL
    ETH2 --> MAIN

    JS -.->|"192.168.20.10<br/>secondary interface"| MAIN

    M1 <-->|"DRBD sync<br/>tcp/7788"| M2
    M1 --- W1
    M1 --- W2
    M2 --- W1
    M2 --- W2

    LB -->|"proxy_pass"| CMS1
    LB -->|"proxy_pass"| CMS2

    MON -.->|"scrape :9100"| M1
    MON -.->|"scrape :9100"| M2
    MON -.->|"scrape :9100"| LB
    MON -.->|"scrape :9100"| CMS1
    MON -.->|"scrape :9100"| CMS2
```

### Legend

| Symbol | Meaning |
|:-------|:--------|
| Solid line (`→`) | Data traffic / active connection |
| Dashed line (`-.->`) | Management / monitoring traffic |
| `⚖` | Load balancer |
| `🟢` | Cluster master node |
| `🔵` | Cluster worker node |
| `📊` | Monitoring node |
| `💻` | Hot-desk workstation |

---

## 2. Service Architecture

Diagram showing the traffic flow from external client to database, including the monitoring and provisioning layers.

```mermaid
graph LR
    subgraph CLIENT["Client"]
        USER["👤 User<br/>web browser"]
    end

    subgraph PERIMETER["Perimeter"]
        ROUTER["🔒 ufw-router<br/>DNAT :80/:443 → LB"]
    end

    subgraph LB_LAYER["Load Balancing Layer"]
        NGINX["⚖ Nginx LB<br/>192.168.20.100<br/>Round-Robin"]
    end

    subgraph WEB_LAYER["Web Layer"]
        APACHE1["🌐 Apache + WP<br/>192.168.20.101"]
        APACHE2["🌐 Apache + WP<br/>192.168.20.102"]
    end

    subgraph DB_LAYER["Data Layer (K3s)"]
        MARIA["🗄 MariaDB 10.11<br/>K3s StatefulSet"]
        DRBD["💾 DRBD 9<br/>Synchronous Replication"]
    end

    subgraph MONITOR["Observability"]
        PROM["📊 Prometheus<br/>192.168.10.20:9090"]
        GRAF["📈 Grafana<br/>192.168.10.20:3000"]
    end

    subgraph PROVISION["Provisioning"]
        COBBLER["🖥 Cobbler + Puppet<br/>192.168.10.10"]
    end

    USER -->|"HTTP/S"| ROUTER
    ROUTER -->|"DNAT"| NGINX
    NGINX -->|"upstream"| APACHE1
    NGINX -->|"upstream"| APACHE2
    APACHE1 -->|"tcp/3306"| MARIA
    APACHE2 -->|"tcp/3306"| MARIA
    MARIA --- DRBD

    PROM -.->|"scrape"| NGINX
    PROM -.->|"scrape"| APACHE1
    PROM -.->|"scrape"| APACHE2
    PROM -.->|"scrape"| MARIA
    GRAF -.->|"datasource"| PROM

    COBBLER -.->|"PXE + Puppet"| APACHE1
    COBBLER -.->|"PXE + Puppet"| APACHE2
    COBBLER -.->|"PXE + Puppet"| NGINX
```

### HTTP Traffic Flow

1. The **client** sends an HTTP/S request to the router's public IP.
2. The **router (UFW)** applies DNAT to redirect traffic on ports 80/443 to the load balancer `main-lb` (192.168.20.100).
3. **Nginx** distributes the request between `main-cms1` and `main-cms2` using round-robin.
4. The **Apache + WordPress** frontend processes the PHP request and queries **MariaDB** in the K3s cluster.
5. **DRBD** maintains synchronous replication of MariaDB data between `internal-master1` and `internal-master2`.

### Monitoring Flow

- **Prometheus** (192.168.10.20:9090) performs periodic scraping (every 15s) of all exporters.
- **Grafana** (192.168.10.20:3000) consumes Prometheus as a datasource and renders operational dashboards.
- **Alertmanager** (192.168.10.20:9093) routes alerts based on severity to configured webhook receivers.

---

## 3. Deployment Sequence

Diagram showing the execution order of deployment scripts when running the solution.

```mermaid
graph TD
    START(["▶ Start Deployment"])

    FASE00A["Phase 00a: Create networks and launch Jumpstart VM<br/>(00_init_vms.sh --jumpstart-only)"]
    WAIT_JS["⏳ Wait for Jumpstart SSH connectivity"]
    FASE01["Phase 01: Install and configure Cobbler on Jumpstart<br/>(00_setup_cobbler.sh)"]
    FASE015["Phase 01.5: Register all nodes in Cobbler<br/>(add_cobbler_nodes.sh)"]
    FASE00B["Phase 00b: Batch client node installation and RAM adjustment<br/>(install_by_batches.sh)"]
    WAIT_NODES["⏳ Wait for all VMs to respond via SSH"]
    FASE018["Phase 01.8: Repair SSH and Puppet CA<br/>(08_repair_ssh_puppet.sh)"]
    FASE02["Phase 02: Deploy Puppet Server and agents<br/>(01_setup_puppet.sh)"]
    FASE07["Phase 03: Configure DRBD HA storage replication<br/>(06_setup_drbd.sh)"]
    FASE04["Phase 04: Deploy K3s HA cluster and MariaDB StatefulSet<br/>(03_setup_kubernetes.sh)"]
    FASE03["Phase 05: Configure Nginx LB and Apache WordPress frontends<br/>(02_setup_nginx.sh)"]
    FASE05["Phase 06: Install Prometheus + Grafana + Alertmanager<br/>(04_setup_monitoring.sh)"]
    FASE06["Phase 07: Apply UFW perimeter and per-node firewall policies<br/>(05_setup_ufw.sh)"]
    FASE08["Phase 08: Deploy internal CA with step-ca<br/>(09_setup_internal_ca.sh)"]

    TRAFFIC["Phase 10: Traffic mix and load testing (optional)<br/>(07_traffic_mix.sh)"]

    END(["✅ Deployment Complete"])

    START --> FASE00A
    FASE00A --> WAIT_JS
    WAIT_JS --> FASE01
    FASE01 --> FASE015
    FASE015 --> FASE00B
    FASE00B --> WAIT_NODES
    WAIT_NODES --> FASE018
    FASE018 --> FASE02
    FASE02 --> FASE07
    FASE07 --> FASE04
    FASE04 --> FASE03
    FASE03 --> FASE05
    FASE05 --> FASE06
    FASE06 --> FASE08
    FASE08 --> END
    END -.->|"optional"| TRAFFIC
```

### Deployment Sequence Notes

- The initial deployment requires launching the **Jumpstart node (Phase 00a)** first, as it serves as the provisioning server for all other clients.
- **Phase 00b** is performed **sequentially in batches** (`install_by_batches.sh`) to prevent the host's physical memory (27 GB) from being exhausted by the OS installers' initial requirements.
- The `deploy_all.sh --skip-vm-create` command orchestrates all phases from **Phase 01.8** onwards, once all client VMs are installed and running with optimised RAM.
- Configuration phases (Puppet, DRBD, K3s, Nginx, UFW, Monitoring, and Internal CA) are **strictly sequential** due to mutual service dependencies.
