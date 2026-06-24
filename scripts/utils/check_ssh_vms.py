#!/usr/bin/env python3
# check_ssh_vms.py
#
#
# Descripción:
#   Script de diagnóstico escrito en Python que verifica de forma estructurada el estado
#   de las máquinas virtuales del clúster. Para cada nodo, comprueba:
#     1. Estado de ejecución según el hipervisor libvirt (virsh).
#     2. Respuesta a ecos ICMP (Ping).
#     3. Disponibilidad del puerto TCP 22 (SSH).
#     4. Capacidad de login interactivo sin clave mediante la llave SSH autorizada.

import os
import subprocess
import socket
import sys

# Listado de todas las VMs registradas con sus direcciones IP asociadas
VMS = [
    {"name": "jumpstart", "ip": "192.168.10.10"},
    {"name": "ufw-router", "ip": "192.168.10.1"},
    {"name": "internal-monitor", "ip": "192.168.10.20"},
    {"name": "internal-storage", "ip": "192.168.10.15"},
    {"name": "internal-master1", "ip": "192.168.10.11"},
    {"name": "internal-master2", "ip": "192.168.10.12"},
    {"name": "internal-worker1", "ip": "192.168.10.13"},
    {"name": "internal-worker2", "ip": "192.168.10.14"},
    {"name": "main-lb", "ip": "192.168.20.100"},
    {"name": "main-cms1", "ip": "192.168.20.101"},
    {"name": "main-cms2", "ip": "192.168.20.102"},
    {"name": "main-hotdesk1", "ip": "192.168.20.201"},
    {"name": "main-hotdesk2", "ip": "192.168.20.202"},
    {"name": "main-hotdesk3", "ip": "192.168.20.203"}
]

def check_ping(ip):
    """Envía un paquete ICMP con timeout de 1 segundo para verificar conectividad IP."""
    try:
        subprocess.check_output(["ping", "-c", "1", "-W", "1", ip], stderr=subprocess.STDOUT)
        return True
    except subprocess.CalledProcessError:
        return False

def check_port_22(ip):
    """Comprueba mediante sockets si el puerto 22 responde a nivel TCP."""
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(1.0)
        s.connect((ip, 22))
        s.close()
        return True
    except Exception:
        return False

def check_ssh_login(ip):
    """Intenta realizar login por SSH como root usando la clave privada autorizada."""
    try:
        cmd = [
            "ssh",
            "-i", os.environ.get("HOST_KEY_FILE", os.path.expanduser("~/.ssh/id_ed25519_gar")),
            "-o", "StrictHostKeyChecking=no",
            "-o", "UserKnownHostsFile=/dev/null",
            "-o", "BatchMode=yes",
            "-o", "ConnectTimeout=2",
            f"root@{ip}",
            "uptime"
        ]
        output = subprocess.check_output(cmd, text=True, stderr=subprocess.DEVNULL)
        return True, output.strip()
    except subprocess.CalledProcessError:
        return False, ""

def get_virsh_state(name):
    """Obtiene el estado de la máquina virtual directamente de libvirt."""
    try:
        state = subprocess.check_output(["sudo", "virsh", "domstate", name], text=True, stderr=subprocess.DEVNULL)
        return state.strip()
    except subprocess.CalledProcessError:
        return "not defined"

def main():
    print(f"{'Nombre VM':<18} | {'Estado KVM':<12} | {'Ping':<5} | {'Port 22':<7} | {'SSH Login':<9} | Detalles / Uptime")
    print("-" * 90)

    total = len(VMS)
    healthy = 0

    for vm in VMS:
        name = vm["name"]
        ip = vm["ip"]
        
        kvm_state = get_virsh_state(name)
        pingable = "OK" if check_ping(ip) else "FALLA"
        port22 = "ABIERTO" if check_port_22(ip) else "CERRADO"
        
        ssh_ok = "FALLA"
        details = ""
        
        if port22 == "ABIERTO":
            ok, uptime_str = check_ssh_login(ip)
            if ok:
                ssh_ok = "OK"
                details = uptime_str
                healthy += 1
            else:
                details = "Puerto abierto, pero falló login (Revisar claves o CA)"
        else:
            if kvm_state == "running":
                details = "VM encendida, pero sin red configurada o kernel booteando"
            elif kvm_state == "shut off":
                details = "VM apagada"
            else:
                details = f"Estado: {kvm_state}"

        print(f"{name:<18} | {kvm_state:<12} | {pingable:<5} | {port22:<7} | {ssh_ok:<9} | {details}")

    print("-" * 90)
    print(f"Resumen: {healthy}/{total} VMs en correcto estado (SSH OK)")

    # Retornar código de error si hay nodos inestables
    if healthy < total:
        sys.exit(1)

if __name__ == "__main__":
    main()
