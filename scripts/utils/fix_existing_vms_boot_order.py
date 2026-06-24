#!/usr/bin/env python3
# fix_existing_vms_boot_order.py
#
#
# Descripción:
#   Script utilitario en Python que parsea los XMLs de definición de libvirt para reordenar
#   el arranque de las máquinas virtuales. Coloca el disco duro ('hd') como primer dispositivo
#   y la red ('network') como secundaria. Esto evita que las VMs ya instaladas intenten realizar
#   un bucle de reinstalación PXE infinito al reiniciarse.

import subprocess
import xml.etree.ElementTree as ET
import sys

# Lista de todas las VMs en la infraestructura CMS
VMS = [
    "ufw-router", "jumpstart",
    "internal-monitor", "internal-master1", "internal-master2",
    "internal-worker1", "internal-worker2", "internal-storage",
    "main-lb", "main-cms1", "main-cms2",
    "main-hotdesk1", "main-hotdesk2", "main-hotdesk3"
]

def fix_vm_boot_order(vm_name):
    """Obtiene el XML de definición de la VM, altera el orden en la sección <os> y lo redefine."""
    try:
        # Dumpear el XML actual del dominio de libvirt
        xml_data = subprocess.check_output(["sudo", "virsh", "dumpxml", vm_name], text=True, stderr=subprocess.DEVNULL)
    except subprocess.CalledProcessError:
        print(f"[-] VM '{vm_name}' no disponible o sin registrar en libvirt. Omitiendo.")
        return

    try:
        root = ET.fromstring(xml_data)
        os_elem = root.find("os")
        if os_elem is None:
            print(f"[-] VM '{vm_name}': Elemento <os> no hallado.")
            return

        boots = os_elem.findall("boot")
        if not boots:
            print(f"[-] VM '{vm_name}': Dispositivos <boot> no definidos.")
            return

        # Comprobar si 'hd' (disco local) ya encabeza el orden
        first_boot = boots[0].get("dev")
        if first_boot == "hd":
            print(f"[=] VM '{vm_name}': Ya arranca prioritariamente por disco ('hd'). Omitiendo.")
            return

        # Desvincular elementos boot temporales
        for b in boots:
            os_elem.remove(b)

        # Ordenar asignando prioridad al disco ('hd'), y dejando el resto de secundario
        new_boots = sorted(boots, key=lambda x: 0 if x.get("dev") == "hd" else 1)
        for b in new_boots:
            os_elem.append(b)

        # Guardar cambios a un XML temporal
        temp_xml_path = f"/tmp/new_{vm_name}.xml"
        tree = ET.ElementTree(root)
        tree.write(temp_xml_path, encoding="utf-8", xml_declaration=True)

        # Redefinir la máquina virtual en libvirt a partir de la nueva definición
        subprocess.check_call(["sudo", "virsh", "define", temp_xml_path], stdout=subprocess.DEVNULL)
        print(f"[✓] VM '{vm_name}': Prioridad de arranque corregida (disco local primero).")

    except Exception as e:
        print(f"[ERROR] Excepción procesando la VM '{vm_name}': {e}")

def main():
    print("==========================================================================")
    print("  Corrigiendo orden de arranque (hd primero) de las VMs existentes")
    print("==========================================================================")
    for vm in VMS:
        fix_vm_boot_order(vm)
    print("==========================================================================")
    print("[OK] ¡Proceso de corrección de arranque finalizado!")

if __name__ == "__main__":
    main()
