"""CI package for a pinned, tools-only runtime. No customer data is accepted."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.request

from step_token_count import ASSETS,REVISION

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--asset-cache',type=Path,help='Optional public assets only; every digest is still checked')
    args=parser.parse_args();out=args.output
    if sys.version_info[:2]!=(3,13):raise RuntimeError('python_313_runtime_required')
    if out.exists():raise RuntimeError('new_bundle_directory_required')
    out.mkdir(parents=True)
    (out/'worker').mkdir();(out/'assets').mkdir()
    here=Path(__file__).resolve().parent
    for name in ('step_token_count.py','step_tool_counter_worker.py'):
        shutil.copyfile(here/name,out/'worker'/name)
    # Only the three packages needed for from_file and the fixed Jinja template.
    subprocess.run([sys.executable,'-m','pip','--isolated','install','--disable-pip-version-check','--no-deps','--no-compile','--only-binary=:all:','--index-url','https://pypi.org/simple','--target',str(out/'python'),'tokenizers==0.22.2','Jinja2==3.1.6','MarkupSafe==3.0.3'],check=True,timeout=120,capture_output=True)
    for name,expected in ASSETS.items():
        url=f'https://huggingface.co/stepfun-ai/Step-3.7-Flash/resolve/{REVISION}/{name}'
        if args.asset_cache is not None:
            shutil.copyfile(args.asset_cache/name,out/'assets'/name)
        else:
            with urllib.request.urlopen(url,timeout=30) as response, (out/'assets'/name).open('xb') as target:
                shutil.copyfileobj(response,target,length=1024*1024)
        with (out/'assets'/name).open('rb') as stream:
            if hashlib.file_digest(stream,'sha256').hexdigest()!=expected:raise RuntimeError('public_asset_digest_mismatch')
    (out/'assets/artifact-manifest.json').write_text(json.dumps({'repository':'stepfun-ai/Step-3.7-Flash','revision':REVISION,'customer_data':False})+'\n')
    files={str(p.relative_to(out)):hashlib.file_digest(p.open('rb'),'sha256').hexdigest() for p in sorted(out.rglob('*')) if p.is_file()}
    (out/'runtime-manifest.json').write_text(json.dumps({'python_major_minor':[3,13],'revision':REVISION,'dependencies':{'tokenizers':'0.22.2','jinja2':'3.1.6','markupsafe':'3.0.3'},'files':files,'customer_data':False},indent=2)+'\n')
    print(json.dumps({'runtime_packaged':True,'public_revision':REVISION,'files':len(files),'customer_data':False}))

if __name__=='__main__':main()
