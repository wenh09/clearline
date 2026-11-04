#!/usr/bin/env python3
"""Include each redistributed Go module's license text and exact source version."""
import json, pathlib, subprocess, sys
go, output=sys.argv[1:]
raw=subprocess.check_output([go,'list','-m','-json','all'],cwd='core',text=True)
decoder=json.JSONDecoder();modules=[]
while raw.strip():
 raw=raw.lstrip();item,end=decoder.raw_decode(raw);raw=raw[end:];modules.append(item)
with open(output,'w') as f:
 f.write('Clearline 0.3.0 — Open-source component notices\n\n')
 for item in modules:
  if item.get('Main'): continue
  folder=pathlib.Path(item.get('Dir',''))
  if not folder.is_dir(): continue
  f.write('\n'+'='*72+'\n'+item['Path']+' '+item.get('Version','')+'\n')
  f.write('Source: https://'+item['Path'].removesuffix('/v2').removesuffix('/v3')+'\n\n')
  candidates=[p for p in folder.iterdir() if p.is_file() and (p.name.upper().startswith('LICENSE') or p.name.upper()=='COPYING' or p.name.upper().startswith('NOTICE'))]
  if not candidates:
   raise SystemExit('Missing license: '+item['Path'])
  for p in sorted(candidates):f.write(p.name+'\n'+p.read_text(errors='replace')+'\n')
