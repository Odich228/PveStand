from pathlib import Path
from tempfile import TemporaryDirectory
import importlib.util
import unittest
import json
import hashlib

ROOT=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('l2',ROOT/'prepare-region-l2.py')
l2=importlib.util.module_from_spec(spec);spec.loader.exec_module(l2)

class TestL2(unittest.TestCase):
 def setup_tree(self,base):
  configs=base/'configs';sysfs=base/'sysfs';configs.mkdir();sysfs.mkdir()
  ids={name:200+i for i,name in enumerate(l2.PORTS)}
  marker='pvestand:'+'a'*32+':'+l2.SCOPE
  for name,ports in l2.PORTS.items():
   lines=[f'name: {name}',f'description: {marker}']
   lines += [f'{nic}: virtio=AA:BB:CC:DD:EE:FF,bridge={bridge},firewall=0,link_down=0' for nic,bridge in ports.items()]
   (configs/f'{ids[name]}.conf').write_text('\n'.join(lines)+'\n')
  for bridge,ends in l2.LINKS.items():
   br=sysfs/bridge;(br/'brif').mkdir(parents=True);(br/'bridge').mkdir()
   (br/'bridge/vlan_filtering').write_text('0');(br/'bridge/stp_state').write_text('0')
   for name,index in ends:
    port=f'tap{ids[name]}i{index}';(br/'brif'/port).touch();bp=sysfs/port/'brport';bp.mkdir(parents=True);(bp/'group_fwd_mask').write_text('0x0')
  return configs,sysfs,ids
 def test_masks_and_pair_counts(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,_=self.setup_tree(Path(d));changes=l2.inspect(cfg,sysfs);self.assertEqual(len(changes),30)
   self.assertEqual(sum(new==0x4004 for _,_,new in changes),4)
   l2.apply_changes(changes)
   self.assertTrue(all(int(path.read_text(),0)==new for path,_,new in changes))
   self.assertTrue(all(old==new for _,old,new in l2.inspect(cfg,sysfs)))
 def test_foreign_extra_port_stops_before_writing(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,_=self.setup_tree(Path(d));(sysfs/'vmbr114/brif/eth0').touch()
   with self.assertRaises(ValueError):l2.inspect(cfg,sysfs)
   self.assertTrue(all(p.read_text()=='0x0' for p in sysfs.glob('tap*/brport/group_fwd_mask')))
 def test_vlan_settings_rejected(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,ids=self.setup_tree(Path(d));p=cfg/f'{ids["GW-1"]}.conf';p.write_text(p.read_text().replace('firewall=0','tag=17,firewall=0'))
   with self.assertRaises(ValueError):l2.inspect(cfg,sysfs)
 def test_unowned_vm_rejected(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,ids=self.setup_tree(Path(d));p=cfg/f'{ids["GW-1"]}.conf';p.write_text(p.read_text().replace('pvestand:', 'someone-else:'))
   with self.assertRaises(ValueError):l2.inspect(cfg,sysfs)
 def test_same_named_template_ignored(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,_=self.setup_tree(Path(d));(cfg/'1001.conf').write_text('name: GW-1\ntemplate: 1\n')
   self.assertEqual(len(l2.inspect(cfg,sysfs)),30)
 def test_existing_mask_bits_preserved(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,_=self.setup_tree(Path(d));first=next(sysfs.glob('tap*/brport/group_fwd_mask'));first.write_text('8')
   change=next(row for row in l2.inspect(cfg,sysfs) if row[0]==first);self.assertTrue(change[2]&8)



class TestMultiL2(unittest.TestCase):
 def fixture(self,base):
  configs=base/'configs';sysfs=base/'sysfs';configs.mkdir();sysfs.mkdir()
  stands=[]
  for index in range(2):
   participant=f'A{index}';scope=hashlib.sha256(f'n/region-{participant}/batch-{participant}'.encode()).hexdigest()[:16]
   names=[f'left-{participant}',f'right-{participant}'];ids={names[0]:200+index*10,names[1]:201+index*10}
   bridges=[f'vmbr{1001+index*100+i}' for i in range(2)]
   ports={name:{f'net{i}':bridge for i,bridge in enumerate(bridges)} for name in names}
   links={bridge:[{'vm':name,'nic':i} for name in names] for i,bridge in enumerate(bridges)}
   for name in names:
    lines=[f'name: {name}',f'description: pvestand:{"a"*32}:{scope}']
    lines += [f'{nic}: e1000=AA:BB:CC:DD:EE:FF,bridge={bridge}' for nic,bridge in ports[name].items()]
    (configs/f'{ids[name]}.conf').write_text('\n'.join(lines)+'\n')
   for i,bridge in enumerate(bridges):
    br=sysfs/bridge;(br/'brif').mkdir(parents=True);(br/'bridge').mkdir()
    (br/'bridge/vlan_filtering').write_text('0');(br/'bridge/stp_state').write_text('0')
    for name in names:
     port=f'tap{ids[name]}i{i}';(br/'brif'/port).touch();bp=sysfs/port/'brport';bp.mkdir(parents=True);(bp/'group_fwd_mask').write_text('8')
   stands.append({'participant':participant,'scope':scope,'ports':ports,'links':links,'masks':{bridge:0x4004 for bridge in bridges}})
  manifest=base/'manifest.json';manifest.write_text(json.dumps({'version':1,'nodes':{'n':stands}}))
  return configs,sysfs,manifest
 def test_all_stands_and_preservation(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,manifest=self.fixture(Path(d));changes=l2.inspect_manifest(manifest,'n',config_dir=cfg,sysfs=sysfs)
   self.assertEqual(len(changes),8);self.assertTrue(all(new==0x400c for _,_,new in changes))
   l2.apply_changes(changes);self.assertTrue(all(old==new for _,old,new in l2.inspect_manifest(manifest,'n',config_dir=cfg,sysfs=sysfs)))
 def test_selected_stand_only(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,manifest=self.fixture(Path(d));changes=l2.inspect_manifest(manifest,'n','A1',cfg,sysfs)
   self.assertEqual(len(changes),4);self.assertTrue(all(path.parent.parent.name.startswith('tap21') for path,_,_ in changes))
 def test_failed_second_stand_does_not_write_first(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,manifest=self.fixture(Path(d));p=cfg/'210.conf';p.write_text(p.read_text().replace('pvestand:','foreign:'))
   with self.assertRaises(ValueError):l2.inspect_manifest(manifest,'n',config_dir=cfg,sysfs=sysfs)
   self.assertTrue(all(p.read_text()=='8' for p in sysfs.glob('tap*/brport/group_fwd_mask')))
   self.assertEqual(len(l2.inspect_manifest(manifest,'n','A0',cfg,sysfs)),4)
 def test_unsafe_manifest_mask_refused(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,manifest=self.fixture(Path(d));data=json.loads(manifest.read_text());data['nodes']['n'][0]['masks']['vmbr1001']=65535;manifest.write_text(json.dumps(data))
   with self.assertRaises(ValueError):l2.inspect_manifest(manifest,'n',config_dir=cfg,sysfs=sysfs)
 def test_wrong_node_refused(self):
  with TemporaryDirectory() as d:
   cfg,sysfs,manifest=self.fixture(Path(d))
   with self.assertRaises(ValueError):l2.inspect_manifest(manifest,'other',config_dir=cfg,sysfs=sysfs)

if __name__=='__main__':unittest.main()
