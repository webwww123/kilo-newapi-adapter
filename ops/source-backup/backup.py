#!/usr/bin/env python3
"""每仓加密保全 Git 历史、安全工作区与差异；默认只预演。"""

import argparse
from collections import Counter
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shlex
import shutil
import stat
import subprocess
import tarfile
import tempfile
import time
import uuid

from verify import MAX_FILE, MIN_FREE, REMOTE_ROOT, KEY, private_dir, prune, refs, sha256, verify_archive, stop_private_agent

LOCAL_ROOT = Path("/opt/audit-source-backups")
LOCK = Path("/run/lock/audit-core-backup.lock")
SSH = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15",
       "-o", "ServerAliveCountMax=3", "-o", "ControlMaster=no", "-o", "ControlPath=none", "50"]
EXCLUDE_DIRS = {
    ".git", "node_modules", ".next", "dist", "target", "logs", "log", "data", "db", "databases",
    ".planning", ".cache", ".venv", "venv", "__pycache__", "coverage", ".turbo", ".nuxt",
    "backups", "backup", "secrets", ".ssh", "cpa-auth", "auth-profiles", "build", "out", "vendor",
}
EXCLUDE_SUFFIXES = {".db", ".sqlite", ".sqlite3", ".log", ".pem", ".key", ".p12", ".pfx",
                    ".jks", ".keystore", ".gpg", ".bundle", ".o", ".a", ".so", ".exe", ".pyc"}
TEMPLATE_ENV = {".env.example", ".env.sample", ".env.template"}
PRIVATE_CONFIG = {"Caddyfile", "Caddyfile.prod", "Caddyfile.production"}
CONFIG_SUFFIXES = {".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".env", ".txt"}


def command(args, *, cwd=None, destination=None, timeout=300, input=None):
    env = dict(os.environ, GIT_OPTIONAL_LOCKS="0")
    if destination is None:
        result = subprocess.run(args, cwd=cwd, env=env, input=input,
                                capture_output=True, timeout=timeout)
    else:
        with open(destination, "xb") as sink:
            result = subprocess.run(args, cwd=cwd, env=env, input=input,
                                    stdout=sink, stderr=subprocess.PIPE, timeout=timeout)
        Path(destination).chmod(0o600)
    if result.returncode:
        raise RuntimeError("backup command failed")
    return result.stdout if destination is None else None


def git(root, *args):
    return command(["git", "-C", str(root), *args])


def fingerprint(st):
    return (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns, st.st_ctime_ns, st.st_mode)


def category(relative, *, directory=False):
    p = PurePosixPath(relative)
    if p.is_absolute() or ".." in p.parts or not p.parts:
        return "unsafe_path"
    if any(part in EXCLUDE_DIRS for part in (p.parts if directory else p.parts[:-1])):
        return "rebuild_or_data"
    name = p.name
    if name in EXCLUDE_DIRS:
        return "rebuild_or_data"
    if name.startswith(".env") and name not in TEMPLATE_ENV:
        return "credential"
    if name in {"config.env", "id_rsa", "id_ed25519", "audit-source-backup.pass", "cpa-config.yaml"}:
        return "credential"
    if name.endswith(("-wal", "-shm", ".bak", ".dump", ".sql.gz", ".tar.gz", ".tgz", ".zip")):
        return "rebuild_or_data"
    if p.suffix.lower() in EXCLUDE_SUFFIXES:
        return "credential" if p.suffix.lower() in {".pem", ".key", ".p12", ".pfx", ".gpg"} else "rebuild_or_data"
    if p.suffix.lower() in CONFIG_SUFFIXES and re.search(
            r"(?:^|[._-])(?:credentials?|secrets?|passwords?|tokens?|auth)(?:[._-]|$)", name.lower()):
        return "credential"
    return "private" if name in PRIVATE_CONFIG else "worktree"


def scan(root):
    """按真实文件系统保存安全工作区，忽略规则不吞掉用户自写代码。"""
    root = Path(root)
    if not root.is_dir() or root.is_symlink() or root.resolve() != root:
        raise RuntimeError("source root must be a real absolute directory")
    entries, omitted = {}, []
    for directory, directories, files in os.walk(root, followlinks=False):
        base = Path(directory)
        kept = []
        for name in sorted(directories):
            p = base / name
            rel = p.relative_to(root).as_posix()
            reason = category(rel, directory=True)
            if p.is_symlink():
                reason = "symlink"
            elif reason == "worktree" and (p / ".git").exists():
                reason = "nested_repository"
            if reason not in {"worktree", "private"}:
                omitted.append({"path": rel, "reason": reason})
            else:
                kept.append(name)
        directories[:] = kept
        for name in sorted(files):
            p = base / name
            rel = p.relative_to(root).as_posix()
            reason = category(rel)
            st = p.lstat()
            if not stat.S_ISREG(st.st_mode):
                reason = "symlink_or_special"
            elif st.st_size > MAX_FILE:
                reason = "oversized"
            if reason not in {"worktree", "private"}:
                omitted.append({"path": rel, "reason": reason})
            else:
                fd = os.open(p, os.O_RDONLY | os.O_NOFOLLOW)
                try:
                    if fingerprint(os.fstat(fd)) != fingerprint(st):
                        raise RuntimeError("source changed during scan")
                    magic = os.read(fd, 4)
                finally:
                    os.close(fd)
                if magic.startswith((b"\x7fELF", b"MZ")):
                    omitted.append({"path": rel, "reason": "compiled_binary"})
                else:
                    entries[rel] = {"category": reason, "stat": fingerprint(st)}
    return entries, omitted


def git_state(root):
    result = subprocess.run(["git", "-C", str(root), "rev-parse", "--show-toplevel"],
                            env=dict(os.environ, GIT_OPTIONAL_LOCKS="0"), capture_output=True, timeout=10)
    if result.returncode:
        return {"enabled": False, "head": None, "refs": []}
    if Path(result.stdout.decode().strip()).resolve() != root:
        raise RuntimeError("source is inside a different Git repository")
    head = subprocess.run(["git", "-C", str(root), "rev-parse", "--verify", "HEAD^{commit}"],
                          capture_output=True, timeout=10)
    index_path = Path(git(root, "rev-parse", "--git-path", "index").decode().strip())
    if not index_path.is_absolute():
        index_path = root / index_path
    return {"enabled": True, "head": head.stdout.decode().strip() if head.returncode == 0 else None,
            "refs": refs(root), "index_sha256": sha256(index_path) if index_path.exists() else None,
            "status_sha256": hashlib.sha256(git(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")).hexdigest()}


def snapshot_file(source, destination, expected):
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        if fingerprint(os.fstat(fd)) != expected:
            raise RuntimeError("source changed before copy")
        with os.fdopen(fd, "rb", closefd=False) as src, destination.open("xb") as sink:
            shutil.copyfileobj(src, sink, 1024 * 1024)
        destination.chmod(0o600)
        if fingerprint(os.fstat(fd)) != expected or fingerprint(Path(source).lstat()) != expected:
            raise RuntimeError("source changed during copy")
    finally:
        os.close(fd)


def collect_patches(root, output, entries, omitted):
    # 新增/删除路径同样检查，生产凭据及链接不写入普通工作区差异。
    index_modes = {}
    for record in git(root, "ls-files", "--stage", "-z").split(b"\0"):
        if record:
            metadata, path = record.split(b"\t", 1)
            index_modes[path.decode(errors="surrogateescape")] = metadata.split()[0]
    for cached, label in [(True, "staged"), (False, "unstaged")]:
        arguments = ["diff", "--name-only", "-z"] + (["--cached"] if cached else [])
        paths = [p.decode(errors="surrogateescape") for p in git(root, *arguments).split(b"\0") if p]
        groups = {"worktree": [], "private": []}
        for path in paths:
            group = category(path)
            if index_modes.get(path) in {b"120000", b"160000"}:
                continue
            if (root / path).exists() and path not in entries:
                continue
            if group in groups:
                groups[group].append(path)
        for group, names in groups.items():
            relative = ("private-config/changes/" if group == "private" else "changes/") + label + ".patch"
            dest = output / relative
            dest.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            if names:
                args = ["git", "-C", str(root), "diff", "--binary", "--no-ext-diff", "--no-textconv"]
                if cached:
                    args.append("--cached")
                command(args + ["--", *names], destination=dest)
            else:
                dest.touch(mode=0o600)
            if dest.stat().st_size > MAX_FILE:
                dest.unlink()
                omitted.append({"path": relative, "reason": "oversized_patch"})


def build_archive(root, work):
    root, work = Path(root), private_dir(work)
    entries, omitted = scan(root)
    before = git_state(root)
    output = private_dir(work / "snapshot")
    for relative, entry in entries.items():
        prefix = "private-config" if entry["category"] == "private" else "worktree"
        snapshot_file(root / relative, output / prefix / relative, entry["stat"])
    if before["enabled"]:
        collect_patches(root, output, entries, omitted)
    if before["head"]:
        bundle = output / "history.bundle"
        git(root, "bundle", "create", str(bundle), "--all", "HEAD")
        bundle.chmod(0o600)
        git(root, "bundle", "verify", str(bundle))
    after_entries, after_omitted = scan(root)
    if entries != after_entries or omitted[:len(after_omitted)] != after_omitted or before != git_state(root):
        raise RuntimeError("source drift; snapshot refused")
    files = {}
    for path in sorted(output.rglob("*")):
        if path.is_file():
            relative = path.relative_to(output).as_posix()
            files[relative] = {"sha256": sha256(path), "size": path.stat().st_size}
            if relative.startswith(("worktree/", "private-config/")) and "/changes/" not in relative:
                original = relative.split("/", 1)[1]
                files[relative]["mode"] = entries[original]["stat"][-1] & 0o777
    manifest = {"format": 1, "source": str(root), "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "git": before, "files": files, "omissions": omitted,
                "omission_counts": dict(Counter(item["reason"] for item in omitted)),
                "deployment_secrets_in_workspace": False}
    (output / "manifest.json").write_text(json.dumps(manifest, sort_keys=True))
    (output / "manifest.json").chmod(0o600)
    archive = work / "source.tar.gz"
    with tarfile.open(archive, "w:gz") as package:
        for relative in ["manifest.json", *sorted(files)]:
            package.add(output / relative, arcname=relative, recursive=False)
    archive.chmod(0o600)
    verify_archive(archive, work / "local-restore")
    # 构建/恢复过程中出现的新修改不得被标为本次成功覆盖。
    final_entries, final_omitted = scan(root)
    if entries != final_entries or final_omitted != after_omitted or before != git_state(root):
        raise RuntimeError("source drift during restore verification")
    return archive, manifest


def ensure_key(path=KEY):
    private_dir(Path(path).parent)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except FileExistsError:
        fd = None
    if fd is not None:
        with os.fdopen(fd, "wb") as sink:
            sink.write((secrets.token_urlsafe(48) + "\n").encode())
            sink.flush()
            os.fsync(sink.fileno())
    st = Path(path).lstat()
    if (not stat.S_ISREG(st.st_mode) or st.st_nlink != 1 or st.st_uid != os.geteuid()
            or st.st_mode & 0o077 or st.st_size < 48 or st.st_size > 1024):
        raise RuntimeError("unsafe encryption key")


def remote_python(script, *arguments, input=None):
    if input is None:
        remote = "python3 - " + " ".join(shlex.quote(str(arg)) for arg in arguments)
        return command(SSH + [remote], input=script.encode(), timeout=900)
    remote = "python3 -c " + shlex.quote(script) + " " + " ".join(shlex.quote(str(arg)) for arg in arguments)
    return command(SSH + [remote], input=input, timeout=60)


KEY_SCRIPT = '''
import os, pathlib, stat, sys
os.umask(0o077)
p=pathlib.Path("/root/secrets/audit-source-backup.pass")
data=sys.stdin.buffer.read(1025)
assert 48 <= len(data) <= 1024
if p.parent.exists():
 st=p.parent.lstat(); assert stat.S_ISDIR(st.st_mode) and st.st_uid == 0 and not st.st_mode & 0o077
else: p.parent.mkdir(mode=0o700)
try: fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
except FileExistsError:
 st=p.lstat(); assert stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and st.st_uid == 0 and not st.st_mode & 0o077
 assert p.read_bytes() == data, "existing remote key mismatch; never overwritten"
else:
 with os.fdopen(fd,"wb") as f: f.write(data); f.flush(); os.fsync(f.fileno())
print("key_ok")
'''

PREPARE_SCRIPT = '''
import os,pathlib,re,shutil,stat,sys
os.umask(0o077)
assert re.fullmatch(r"[a-zA-Z0-9_.-]{1,100}-[a-f0-9]{12}",sys.argv[1])
root=pathlib.Path("/opt/backup-archive/node119/audit-source")
for p in [root,root/sys.argv[1]]:
 if p.exists():
  s=p.lstat(); assert stat.S_ISDIR(s.st_mode) and s.st_uid == 0 and not s.st_mode & 0o077
 else: p.mkdir(parents=True,mode=0o700);p.chmod(0o700)
assert shutil.disk_usage(root).free >= 10*1024**3
print("ready")
'''


def repository_id(root):
    name = re.sub(r"[^a-zA-Z0-9_.-]", "_", root.name)[:80] or "source"
    return name + "-" + hashlib.sha256(str(root).encode()).hexdigest()[:12]


def backup_repository(root):
    repo_id = repository_id(root)
    directory = private_dir(private_dir(LOCAL_ROOT) / repo_id)
    if shutil.disk_usage(directory).free < MIN_FREE:
        raise RuntimeError("local free space below 10 GiB")
    remote_python(PREPARE_SCRIPT, repo_id)
    name = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8] + ".tar.gpg"
    destination = directory / name
    with tempfile.TemporaryDirectory(prefix=".snapshot-", dir=directory) as work:
        Path(work).chmod(0o700)
        archive, manifest = build_archive(root, Path(work))
        if shutil.disk_usage(directory).free < MIN_FREE:
            raise RuntimeError("local free space below 10 GiB during backup")
        private_dir(Path(work) / "gnupg")
        try:
            command(["gpg", "--no-options", "--batch", "--quiet", "--pinentry-mode", "loopback",
                     "--no-symkey-cache", "--homedir", str(Path(work) / "gnupg"),
                     "--passphrase-file", str(KEY), "--symmetric", "--cipher-algo", "AES256",
                     "--output", str(destination), str(archive)])
        finally:
            stop_private_agent(Path(work) / "gnupg")
        destination.chmod(0o600)
    digest = sha256(destination)
    remote_target = str(REMOTE_ROOT / repo_id / name)
    command(["scp", "-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
             "-o", "ControlMaster=no", "-o", "ControlPath=none",
             str(destination), "50:" + remote_target], timeout=900)
    script = Path(__file__).with_name("verify.py").read_text()
    receipt = json.loads(remote_python(script, repo_id, name, digest))
    if not receipt.get("verified") or receipt.get("sha256") != digest:
        raise RuntimeError("remote restore verification failed")
    if sha256(destination) != digest:
        raise RuntimeError("local encrypted package changed")
    receipt["bytes"] = destination.stat().st_size
    with (directory / (name + ".verified.json")).open("x") as sink:
        json.dump(receipt, sink, sort_keys=True)
    prune(directory)
    return {"repository": repo_id, "verified": True, "bytes": receipt["bytes"],
            "workspace_files": sum(k.startswith("worktree/") for k in manifest["files"]),
            "private_config_files": sum(k.startswith("private-config/") for k in manifest["files"]),
            "history_preserved": bool(manifest["git"]["head"]), "omissions": manifest["omission_counts"]}


def read_config(path):
    config = json.loads(Path(path).read_text())
    if set(config) != {"source_node", "local_root", "remote", "retention_verified", "sources"}:
        raise RuntimeError("invalid config fields")
    if (config["source_node"] != "119" or config["local_root"] != str(LOCAL_ROOT)
            or config["remote"] != {"host": "50", "root": str(REMOTE_ROOT)}
            or config["retention_verified"] != 7):
        raise RuntimeError("invalid backup scope")
    sources = [Path(value) for value in config["sources"]]
    if not sources or len(sources) != len(set(sources)):
        raise RuntimeError("invalid source list")
    if any(not p.is_absolute() or ".." in p.parts or not str(p).startswith(("/root/", "/opt/")) for p in sources):
        raise RuntimeError("invalid source path")
    return sources


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default=str(Path(__file__).with_name("sources.json")))
    parser.add_argument("--only", action="append", default=[])
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    sources = read_config(args.config)
    if args.only:
        if any(Path(p) not in sources for p in args.only):
            raise RuntimeError("requested source outside inventory")
        sources = [p for p in sources if str(p) in args.only]
    if not args.apply:
        for root in sources:
            entries, omitted = scan(root)
            state = git_state(root)
            print(json.dumps({"preview": True, "repository": repository_id(root), "git": state["enabled"],
                              "has_commit": bool(state["head"]), "workspace_files": len(entries),
                              "omissions": dict(Counter(x["reason"] for x in omitted))}, sort_keys=True))
        return 0
    if os.geteuid() != 0:
        raise RuntimeError("apply requires root")
    lock_fd = os.open(LOCK, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "w") as lock:
        deadline = time.monotonic() + 600
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("backup lock busy for 10 minutes")
                time.sleep(1)
        ensure_key()
        remote_python(KEY_SCRIPT, input=KEY.read_bytes())
        failed = False
        for root in sources:
            try:
                print(json.dumps(backup_repository(root), sort_keys=True), flush=True)
            except Exception as error:
                failed = True
                print(json.dumps({"repository": repository_id(root), "verified": False,
                                  "error_type": type(error).__name__}), flush=True)
        return 1 if failed else 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps({"verified": False, "error_type": type(error).__name__}))
        raise SystemExit(1)
