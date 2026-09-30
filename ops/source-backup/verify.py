#!/usr/bin/env python3
"""只校验 root-only 源码备份，不打印源码、差异、密钥或仓库远端。"""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile

MAX_FILE = 16 * 1024 * 1024
MIN_FREE = 10 * 1024**3
REMOTE_ROOT = Path("/opt/backup-archive/node119/audit-source")
KEY = Path("/root/secrets/audit-source-backup.pass")
NAME = re.compile(r"^\d{8}T\d{6}Z-[a-f0-9]{8}\.tar\.gpg$")
REPO_ID = re.compile(r"^[a-zA-Z0-9_.-]{1,100}-[a-f0-9]{12}$")


def run(args, timeout=300):
    result = subprocess.run(args, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("verification command failed")
    return result.stdout


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def stop_private_agent(home):
    # 仅清本任务独占的临时 homedir，绝不使用用户默认 GPG 目录。
    run(["gpgconf", "--homedir", str(home), "--kill", "gpg-agent"], timeout=10)


def private_dir(path):
    path = Path(path)
    if path.exists():
        st = path.lstat()
        if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o077:
            raise RuntimeError("unsafe backup directory")
    else:
        path.mkdir(parents=True, mode=0o700)
        path.chmod(0o700)
    return path


def refs(repository):
    value = run(["git", "-C", str(repository), "for-each-ref", "--format=%(refname) %(objectname)"])
    return sorted(value.decode().splitlines())


def verify_archive(archive, target):
    """手工解包普通文件，拒绝链接/绝对路径/重复成员，再逐文件核验。"""
    target = private_dir(target)
    members = {}
    with tarfile.open(archive, "r:gz") as package:
        for member in package.getmembers():
            p = PurePosixPath(member.name)
            if (not member.isfile() or p.is_absolute() or ".." in p.parts
                    or str(p) != member.name or member.name in members):
                raise RuntimeError("unsafe archive member")
            if member.size > MAX_FILE and member.name != "history.bundle":
                raise RuntimeError("unexpected oversized archive member")
            if member.name != "manifest.json" and not (
                    member.name == "history.bundle" or member.name.startswith(
                        ("worktree/", "private-config/", "changes/"))):
                raise RuntimeError("unexpected archive member")
            members[member.name] = member
        if "manifest.json" not in members:
            raise RuntimeError("manifest missing")
        manifest = json.load(package.extractfile(members["manifest.json"]))
        expected = manifest["files"]
        if set(members) != set(expected) | {"manifest.json"}:
            raise RuntimeError("archive manifest mismatch")
        for name, metadata in expected.items():
            member = members[name]
            if member.size != metadata["size"]:
                raise RuntimeError("archive size mismatch")
            destination = target / name
            destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with package.extractfile(member) as source, destination.open("xb") as sink:
                shutil.copyfileobj(source, sink, 1024 * 1024)
            destination.chmod(0o600)
            if sha256(destination) != metadata["sha256"]:
                raise RuntimeError("archive hash mismatch")
    bundle = target / "history.bundle"
    git_meta = manifest["git"]
    if bundle.exists():
        empty = target / "verify.git"
        run(["git", "init", "--bare", str(empty)])
        run(["git", "-C", str(empty), "bundle", "verify", str(bundle)])
        restored = target / "restore.git"
        run(["git", "clone", "--mirror", str(bundle), str(restored)])
        if refs(restored) != git_meta["refs"]:
            raise RuntimeError("restored refs mismatch")
        run(["git", "-C", str(restored), "cat-file", "-e", git_meta["head"] + "^{commit}"])
    elif git_meta.get("head"):
        raise RuntimeError("history bundle missing")
    return manifest


def prune(directory, keep=7):
    """只回收有成功校验凭证的本任务备份，失败包及其他文件不动。"""
    records = []
    for receipt in directory.glob("*.verified.json"):
        if receipt.is_symlink() or receipt.stat().st_size > 16384:
            continue
        try:
            record = json.loads(receipt.read_text())
            name = record["artifact"]
            artifact = directory / name
            if (not NAME.fullmatch(name) or not record.get("verified")
                    or receipt.name != name + ".verified.json" or not artifact.is_file()
                    or artifact.is_symlink() or artifact.stat().st_nlink != 1):
                continue
            records.append((name, receipt, artifact, record["sha256"]))
        except (ValueError, KeyError, OSError):
            continue
    for _, receipt, artifact, digest in sorted(records, reverse=True)[keep:]:
        if sha256(artifact) != digest:
            raise RuntimeError("old backup changed; retention refused")
        artifact.unlink()
        receipt.unlink()


def remote_verify(repo_id, artifact_name, expected_sha):
    if os.geteuid() != 0 or not REPO_ID.fullmatch(repo_id) or not NAME.fullmatch(artifact_name):
        raise RuntimeError("invalid remote verification target")
    directory = private_dir(private_dir(REMOTE_ROOT) / repo_id)
    artifact = directory / artifact_name
    if shutil.disk_usage(directory).free < MIN_FREE:
        raise RuntimeError("remote free space below 10 GiB")
    artifact_st = artifact.lstat()
    if (not stat.S_ISREG(artifact_st.st_mode) or artifact_st.st_uid != 0
            or artifact_st.st_mode & 0o077 or artifact_st.st_nlink != 1
            or sha256(artifact) != expected_sha):
        raise RuntimeError("remote encrypted artifact mismatch")
    key_st = KEY.lstat()
    if not stat.S_ISREG(key_st.st_mode) or key_st.st_uid != 0 or key_st.st_mode & 0o077:
        raise RuntimeError("unsafe remote encryption key")
    with tempfile.TemporaryDirectory(prefix=".restore-", dir=directory) as work:
        Path(work).chmod(0o700)
        plain = Path(work) / "source.tar.gz"
        private_dir(Path(work) / "gnupg")
        try:
            run(["gpg", "--no-options", "--batch", "--quiet", "--pinentry-mode", "loopback",
                 "--no-symkey-cache", "--homedir", str(Path(work) / "gnupg"),
                 "--passphrase-file", str(KEY), "--output", str(plain), "--decrypt", str(artifact)])
        finally:
            stop_private_agent(Path(work) / "gnupg")
        plain.chmod(0o600)
        manifest = verify_archive(plain, Path(work) / "verified")
    if sha256(artifact) != expected_sha:
        raise RuntimeError("remote artifact changed during validation")
    receipt = {"verified": True, "artifact": artifact_name, "sha256": expected_sha,
               "files": len(manifest["files"]), "omissions": manifest["omission_counts"]}
    destination = directory / (artifact_name + ".verified.json")
    with destination.open("x") as sink:
        json.dump(receipt, sink, sort_keys=True)
    destination.chmod(0o600)
    prune(directory)
    return receipt


if __name__ == "__main__":
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument("repo_id")
    parser.add_argument("artifact")
    parser.add_argument("sha256")
    args = parser.parse_args()
    try:
        print(json.dumps(remote_verify(args.repo_id, args.artifact, args.sha256), sort_keys=True))
    except Exception as error:
        # 不传播 gpg/git/SSH 原始报错，以免带出私人仓库信息或源码。
        print(json.dumps({"verified": False, "error_type": type(error).__name__}))
        raise SystemExit(1)
