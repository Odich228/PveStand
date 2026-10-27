# Региональный стенд L2

Пример: `stand.region.l2.example.yaml`. Узел SF-4, пул region1, 15 ВМ, 15 внутренних мостов vmbr101–vmbr115 и внешний vmbr0. Карта соединений — [region-l2-ports.csv](region-l2-ports.csv).

Каждый внутренний мост — отдельный виртуальный кабель с двумя концами. Номера vmbr не задают VLAN ID. VLAN, access/trunk, IP и LACP настраивают конкурсанты внутри гостевых ОС. Два кабеля vmbr114/vmbr115 соединяют PROVIDER-SW и PROVIDER-GW для будущего LACP.

Шаблон EcoRouter 1001 используется для GW-1/GW-2/GW-3/COD-GW/PROVIDER-GW; все их сетевые адаптеры имеют модель e1000. ZSPD-GW использует шаблон 1004. Остальные ВМ используют virtio. replace_nics удаляет неописанные адаптеры; tag/trunks снимаются, firewall и link_down выключаются.

Пользователь Competitor-A0@pve получает роль PVEVMUser на пул region1. Пароль читается из REGION_COMPETITOR_PASSWORD. Настоящие пароли и токены в примере отсутствуют.

```bash
cp stand.region.l2.example.yaml stand.region.l2.yaml
# Задайте REGION_COMPETITOR_PASSWORD локально, а также подключение PVE_HOST/PVE_TOKEN или профиль.
./pvestand -c stand.region.l2.yaml -run validate
./pvestand -c stand.region.l2.yaml -run doctor
./pvestand -c stand.region.l2.yaml -run plan
./pvestand -c stand.region.l2.yaml -run deploy -parallel 1
```

Запускайте из одной папки и сохраняйте .pvestand. Смена template в YAML не пересоздаёт существующую ВМ. Для применения другой модели NIC существующую ВМ нужно предварительно остановить. Комментарии уже существующих мостов автоматически не обновляются.

После полного развёртывания перенесите `scripts/prepare-region-l2.py` на SF-4 и запустите от root:

```bash
python3 prepare-region-l2.py --check
python3 prepare-region-l2.py --apply
python3 prepare-region-l2.py --check
```

Скрипт проверяет владельцев ВМ, карту портов, отсутствие VLAN/tag/trunks, ровно два ожидаемых TAP-порта на каждом мосту и отключённые vlan_filtering/STP. Затем разрешает LLDP на внутренних TAP-портах и LACP на четырёх портах vmbr114/vmbr115. WAN и физические интерфейсы не изменяются. После остановки/запуска ВМ TAP создаются заново: скрипт нужно повторить. При ошибке записи скрипт пытается вернуть прежние маски.

Реальный доступ в Internet через vmbr0, аппаратная совместимость шаблонов и гостевая конфигурация проверяются отдельно.
