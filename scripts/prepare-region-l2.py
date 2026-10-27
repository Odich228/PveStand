#!/usr/bin/env python3
"""Run on SF-4 after deployment. Pass LACP/LLDP through the lab's TAP ports.
No VLAN, bond, guest configuration, physical port, or WAN changes are made.
Default mode only checks. --apply changes group_fwd_mask after all checks pass.
Re-run after restarting VMs, because TAP ports are recreated.
"""
import argparse
import hashlib
import json
import os
import re
import sys
from pathlib import Path
from urllib.parse import unquote

LINKS = {'vmbr101': [('GW-CORE', 1), ('GW-1', 0)], 'vmbr102': [('GW-CORE', 2), ('GW-2', 0)], 'vmbr103': [('GW-CORE', 3), ('GW-3', 0)], 'vmbr104': [('GW-1', 1), ('COD-GW', 0)], 'vmbr105': [('COD-GW', 1), ('COD-SW-CORE', 0)], 'vmbr106': [('COD-SW-CORE', 1), ('COD-ADMIN', 0)], 'vmbr107': [('COD-SW-CORE', 2), ('DATA', 0)], 'vmbr108': [('COD-SW-CORE', 3), ('COD-SRV', 0)], 'vmbr109': [('COD-SW-CORE', 4), ('PUB', 0)], 'vmbr110': [('COD-SW-CORE', 5), ('NODE', 0)], 'vmbr111': [('GW-2', 1), ('ZSPD-SW', 0)], 'vmbr112': [('ZSPD-SW', 1), ('ZSPD-GW', 0)], 'vmbr113': [('GW-3', 1), ('PROVIDER-SW', 0)], 'vmbr114': [('PROVIDER-SW', 1), ('PROVIDER-GW', 0)], 'vmbr115': [('PROVIDER-SW', 2), ('PROVIDER-GW', 1)]}
PORTS = {'GW-CORE': {'net0': 'vmbr0', 'net1': 'vmbr101', 'net2': 'vmbr102', 'net3': 'vmbr103'}, 'GW-1': {'net0': 'vmbr101', 'net1': 'vmbr104'}, 'GW-2': {'net0': 'vmbr102', 'net1': 'vmbr111'}, 'GW-3': {'net0': 'vmbr103', 'net1': 'vmbr113'}, 'PROVIDER-SW': {'net0': 'vmbr113', 'net1': 'vmbr114', 'net2': 'vmbr115'}, 'PROVIDER-GW': {'net0': 'vmbr114', 'net1': 'vmbr115'}, 'ZSPD-SW': {'net0': 'vmbr111', 'net1': 'vmbr112'}, 'ZSPD-GW': {'net0': 'vmbr112'}, 'COD-GW': {'net0': 'vmbr104', 'net1': 'vmbr105'}, 'COD-SW-CORE': {'net0': 'vmbr105', 'net1': 'vmbr106', 'net2': 'vmbr107', 'net3': 'vmbr108', 'net4': 'vmbr109', 'net5': 'vmbr110'}, 'COD-ADMIN': {'net0': 'vmbr106'}, 'COD-SRV': {'net0': 'vmbr108'}, 'DATA': {'net0': 'vmbr107'}, 'PUB': {'net0': 'vmbr109'}, 'NODE': {'net0': 'vmbr110'}}

SCOPE = hashlib.sha256(b"SF-4/region1/region-stand1").hexdigest()[:16]
CONFIG_DIR = Path("/etc/pve/local/qemu-server")
SYSFS = Path("/sys/class/net")


def properties(value):
    return dict(part.split("=", 1) for part in value.split(",") if "=" in part)


def config(path):
    values = {}
    for line in path.read_text().splitlines():
        if line.startswith("["):
            break  # current configuration only, never snapshot/PENDING sections
        if line.startswith("#") or ":" not in line:
            continue
        key, value = line.split(":", 1)
        values[key.strip()] = value.strip()
    return values


def inspect(config_dir=CONFIG_DIR, sysfs=SYSFS, *, ports=None, links=None, scope=None, masks=None):
    ports = PORTS if ports is None else ports
    links = LINKS if links is None else links
    scope = SCOPE if scope is None else scope
    found = {}
    owners = set()
    for path in config_dir.glob("*.conf"):
        cfg = config(path)
        name = cfg.get("name")
        if name not in ports or cfg.get("template") == "1":
            continue
        if name in found:
            raise ValueError(f"Несколько обычных ВМ с именем {name}: проверьте VMID")
        if not re.fullmatch(r"pvestand:[0-9a-f]{32}:" + scope, unquote(cfg.get("description", ""))):
            raise ValueError(f"{name}: нет метки владельца данного стенда; настройки не изменены")
        owners.add(unquote(cfg["description"]))
        found[name] = (int(path.stem), cfg)
    missing = sorted(set(ports) - set(found))
    if missing:
        raise ValueError("Сначала разверните все ВМ: отсутствуют " + ", ".join(missing))
    if len(owners) != 1:
        raise ValueError("ВМ принадлежат разным журналам; настройки не изменены")
    for name, expected in ports.items():
        _, cfg = found[name]
        actual_keys = {k for k in cfg if re.fullmatch(r"net[0-9]+", k)}
        if actual_keys != set(expected):
            raise ValueError(f"{name}: набор NIC отличается от схемы ({sorted(actual_keys)})")
        for key, bridge in expected.items():
            p = properties(cfg[key])
            if p.get("bridge") != bridge:
                raise ValueError(f"{name} {key}: ожидается {bridge}")
            if "tag" in p or "trunks" in p:
                raise ValueError(f"{name} {key}: обнаружен VLAN tag/trunks на Proxmox")
            if p.get("firewall", "0") != "0" or p.get("link_down", "0") != "0":
                raise ValueError(f"{name} {key}: требуется firewall=0 и link_down=0")
    changes = []
    for bridge, endpoints in links.items():
        br = sysfs / bridge
        if (br / "bridge/vlan_filtering").read_text().strip() != "0":
            raise ValueError(f"{bridge}: нужен vlan_filtering=0 для прозрачного учебного кабеля")
        if (br / "bridge/stp_state").read_text().strip() != "0":
            raise ValueError(f"{bridge}: отключите STP хоста для данного учебного кабеля")
        expected = {f"tap{found[name][0]}i{index}" for name, index in endpoints}
        actual = {p.name for p in (br / "brif").iterdir()}
        if actual != expected:
            raise ValueError(f"{bridge}: ожидаются ровно {sorted(expected)}, подключены {sorted(actual)}; проверьте запуск ВМ и firewall")
        mask = masks[bridge] if masks is not None else (0x4004 if bridge in {"vmbr114", "vmbr115"} else 0x4000)
        for port in sorted(expected):
            path = sysfs / port / "brport/group_fwd_mask"
            old = int(path.read_text().strip(), 0)
            changes.append((path, old, old | mask))
    return changes


def apply_changes(changes):
    applied = []
    try:
        for path, old, new in changes:
            if old == new:
                continue
            # Add before writing, so even a readback failure triggers rollback.
            applied.append((path, old))
            path.write_text(str(new) + "\n")
            if int(path.read_text().strip(), 0) != new:
                raise ValueError(f"{path}: проверка записи не прошла")
    except Exception as exc:
        rollback_errors = []
        for path, old in reversed(applied):
            try:
                path.write_text(str(old) + "\n")
            except Exception as rollback_error:
                rollback_errors.append(str(rollback_error))
        detail = "; ошибки восстановления: " + "; ".join(rollback_errors) if rollback_errors else ""
        raise ValueError(f"Не удалось применить настройки: {exc}{detail}") from exc


def inspect_manifest(path, node, participant=None, config_dir=CONFIG_DIR, sysfs=SYSFS):
    manifest = json.loads(Path(path).read_text())
    if manifest.get("version") != 1 or not isinstance(manifest.get("nodes"), dict):
        raise ValueError("Неподдерживаемый формат карты L2")
    stands = manifest["nodes"].get(node, [])
    if participant is not None:
        stands = [stand for stand in stands if stand.get("participant") == participant]
    if not stands:
        raise ValueError("В карте L2 нет выбранных стендов на узле " + node)
    changes, seen_bridges, seen_names = [], set(), set()
    for stand in stands:
        scope, ports, raw_links, masks = (stand[key] for key in ("scope", "ports", "links", "masks"))
        if not re.fullmatch(r"[0-9a-f]{16}", scope) or not ports or not raw_links:
            raise ValueError("Пустая/неверная схема стенда")
        links = {}
        expected_links = {}
        for name, adapters in ports.items():
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]*", name) or name in seen_names:
                raise ValueError("Повторяющееся/неверное имя ВМ в карте L2")
            seen_names.add(name)
            for adapter, bridge in adapters.items():
                if not re.fullmatch(r"net[0-9]+", adapter) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", bridge):
                    raise ValueError("Неверный порт или bridge в карте L2")
                expected_links.setdefault(bridge, set()).add((name, int(adapter[3:])))
        if set(raw_links) != set(masks):
            raise ValueError("Наборы мостов и масок не совпадают")
        for bridge, endpoints in raw_links.items():
            if not re.fullmatch(r"vmbr[0-9]{1,4}", bridge) or bridge in seen_bridges or masks[bridge] not in (0x4000, 0x4004):
                raise ValueError("Повторяющийся/неверный bridge или маска в карте L2")
            seen_bridges.add(bridge)
            pairs = [(end["vm"], end["nic"]) for end in endpoints]
            if any(not isinstance(nic, int) or nic < 0 for _, nic in pairs):
                raise ValueError("Неверный индекс адаптера")
            if len(pairs) != 2 or pairs[0][0] == pairs[1][0] or set(pairs) != expected_links.get(bridge):
                raise ValueError("Соединения и порты не совпадают в карте L2")
            links[bridge] = pairs
        # Every selected stand is checked before the caller writes any mask.
        changes.extend(inspect(config_dir, sysfs, ports=ports, links=links, scope=scope, masks=masks))
    return changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--apply", action="store_true", help="проверить схему и разрешить LLDP/LACP на TAP-портах")
    mode.add_argument("--check", action="store_true", help="только проверить (по умолчанию)")
    parser.add_argument("--manifest", help="JSON из pvestand -run l2manifest")
    parser.add_argument("--node", default=os.uname().nodename.split(".")[0], help="имя текущего узла в карте")
    parser.add_argument("--participant", help="проверить/настроить только один стенд, например A0")
    args = parser.parse_args()
    if args.participant and not args.manifest:
        parser.error("--participant требует --manifest")
    if args.apply and args.node != os.uname().nodename.split(".")[0]:
        parser.error("--node должен совпадать с текущим узлом")
    if args.apply and os.geteuid() != 0:
        parser.error("--apply требуется запускать от root на целевом узле")
    try:
        changes = inspect_manifest(args.manifest, args.node, args.participant) if args.manifest else inspect()
        pending = sum(old != new for _, old, new in changes)
        if args.apply:
            apply_changes(changes)
        for path, old, new in changes:
            actual = int(path.read_text().strip(), 0)
            print(f"{path.parent.parent.name}: {actual:#06x}, требуется {new:#06x}")
        if args.apply or pending == 0:
            print("Проверено: схема и маски LLDP/LACP выбранных стендов соответствуют карте L2.")
            print("Настройку VLAN и самого LACP выполняют конкурсанты в гостевых устройствах.")
            return 0
        print(f"Нужна настройка {pending} TAP-портов. Повторите с --apply.")
        return 2
    except (OSError, ValueError, KeyError, TypeError) as exc:
        print("Ошибка: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
