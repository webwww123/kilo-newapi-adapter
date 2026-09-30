#!/usr/bin/env python3
"""只校验 root-only 源码备份，不打印源码、差异、密钥或仓库远端。"""

import argparse
from contextlib import contextmanager
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

# 仅允许程序自定义的固定错误码进入报告，外部命令报错绝不转发。
SAFE_ERRORS = {
    "backup command failed": "backup_command_failed",
    "verification command failed": "verification_command_failed",
    "source root must be a real absolute directory": "source_root_invalid",
    "source changed during scan": "source_scan_drift",
    "source is inside a different Git repository": "source_repository_mismatch",
    "source changed before copy": "source_copy_drift",
    "source changed during copy": "source_copy_drift",
    "source drift; snapshot refused": "source_snapshot_drift",
    "source drift during restore verification": "source_restore_drift",
    "unsafe backup directory": "backup_directory_unsafe",
    "unsafe archive member": "archive_member_unsafe",
    "unexpected oversized archive member": "archive_member_oversized",
    "unexpected archive member": "archive_member_unexpected",
    "manifest missing": "archive_manifest_missing",
    "archive manifest mismatch": "archive_manifest_mismatch",
    "archive size mismatch": "archive_size_mismatch",
    "archive hash mismatch": "archive_hash_mismatch",
    "restored refs mismatch": "restored_refs_mismatch",
    "history bundle missing": "history_missing",
    "old backup changed; retention refused": "retention_artifact_changed",
    "invalid remote verification target": "remote_target_invalid",
    "remote free space below 10 GiB": "remote_space_low",
    "remote encrypted artifact mismatch": "remote_artifact_mismatch",
    "unsafe remote encryption key": "remote_key_unsafe",
    "remote artifact changed during validation": "remote_artifact_drift",
    "unsafe encryption key": "encryption_key_unsafe",
    "local free space below 10 GiB": "local_space_low",
    "local free space below 10 GiB during backup": "local_space_low",
    "remote restore verification failed": "remote_restore_failed",
    "local encrypted package changed": "local_artifact_drift",
    "invalid config fields": "config_fields_invalid",
    "invalid backup scope": "backup_scope_invalid",
    "invalid source list": "source_list_invalid",
    "invalid source path": "source_path_invalid",
    "requested source outside inventory": "source_outside_inventory",
    "apply requires root": "root_required",
    "backup lock busy for 10 minutes": "backup_lock_busy",
}
SAFE_CODES = frozenset(SAFE_ERRORS.values()) | {
    "git_shallow_clone_failed", "git_history_check_failed", "git_history_dependency",
    "git_history_member_unsafe", "git_history_head_mismatch", "git_history_shallow_mismatch",
    "git_history_format_mismatch", "gpg_encrypt_failed", "gpg_decrypt_failed",
    "gpg_agent_cleanup_failed", "history_format_mismatch",
}


def safe_error(error):
    detail = error.args[0] if type(error) is RuntimeError and len(error.args) == 1 else None
    code = SAFE_ERRORS.get(detail) if isinstance(detail, str) else None
    if code is None and isinstance(detail, str) and detail in SAFE_CODES:
        code = detail
    if code is None:
        code = "command_timeout" if isinstance(error, subprocess.TimeoutExpired) else "unexpected_error"
    return {"error_type": type(error).__name__, "error_code": code}


def run(args, timeout=300, *, failure="verification command failed"):
    try:
        result = subprocess.run(args, capture_output=True, timeout=timeout,
                                env=dict(os.environ, GIT_OPTIONAL_LOCKS="0", GIT_NO_LAZY_FETCH="1"))
    except subprocess.TimeoutExpired:
        raise RuntimeError(failure) from None
    if result.returncode:
        raise RuntimeError(failure)
    return result.stdout


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def stop_private_agent(home):
    # 仅清本任务独占的临时 homedir，绝不使用用户默认 GPG 目录。
    run(["gpgconf", "--homedir", str(home), "--kill", "gpg-agent"], timeout=10,
        failure="gpg_agent_cleanup_failed")


@contextmanager
def temporary_gpg_home():
    # Unix socket 的 .extra/.browser 后缀也受长度限制，勿放在长仓名下。
    with tempfile.TemporaryDirectory(prefix="audit-gpg-", dir="/tmp") as name:
        home = private_dir(name)
        try:
            yield home
        finally:
            stop_private_agent(home)


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


def verify_shallow_history(repository, git_meta):
    """仅验收本机已有的浅历史，禁止依赖源仓或自动补取上游对象。"""
    repository = Path(repository)
    if (repository / "objects/info/alternates").exists() or list((repository / "objects/pack").glob("*.promisor")):
        raise RuntimeError("git_history_dependency")
    # tar 只保存普通文件，packed-refs 仓库的空 refs 目录需在副本中重建。
    for name in ("objects", "refs"):
        (repository / name).mkdir(mode=0o700, exist_ok=True)
    args = ["git", "-C", str(repository)]
    if run(args + ["rev-parse", "--is-bare-repository"], failure="git_history_check_failed").strip() != b"true":
        raise RuntimeError("git_history_format_mismatch")
    if run(args + ["rev-parse", "--is-shallow-repository"], failure="git_history_check_failed").strip() != b"true":
        raise RuntimeError("git_history_shallow_mismatch")
    shallow = repository / "shallow"
    if not shallow.is_file() or sha256(shallow) != git_meta.get("shallow_sha256"):
        raise RuntimeError("git_history_shallow_mismatch")
    if refs(repository) != git_meta["refs"]:
        raise RuntimeError("restored refs mismatch")
    if run(args + ["rev-parse", "--verify", "HEAD^{commit}"], failure="git_history_check_failed").decode().strip() != git_meta["head"]:
        raise RuntimeError("git_history_head_mismatch")
    head = (repository / "HEAD").read_text().strip()
    expected = "ref: " + git_meta["symbolic_head"] if git_meta.get("symbolic_head") else git_meta["head"]
    if head != expected:
        raise RuntimeError("git_history_head_mismatch")
    run(args + ["fsck", "--connectivity-only", "--no-reflogs"], failure="git_history_check_failed")


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
            if member.size > MAX_FILE and member.name != "history.bundle" and not member.name.startswith("history.git/objects/"):
                raise RuntimeError("unexpected oversized archive member")
            if member.name != "manifest.json" and not (
                    member.name == "history.bundle" or member.name.startswith(
                        ("worktree/", "private-config/", "changes/", "history.git/"))):
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
    history = target / "history.git"
    git_meta = manifest["git"]
    if history.exists():
        if bundle.exists() or not git_meta.get("shallow") or not git_meta.get("head"):
            raise RuntimeError("history_format_mismatch")
        verify_shallow_history(history, git_meta)
    elif bundle.exists():
        if git_meta.get("shallow"):
            raise RuntimeError("history_format_mismatch")
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
        with temporary_gpg_home() as home:
            run(["gpg", "--no-options", "--batch", "--quiet", "--pinentry-mode", "loopback",
                 "--no-symkey-cache", "--homedir", str(home),
                 "--passphrase-file", str(KEY), "--output", str(plain), "--decrypt", str(artifact)],
                failure="gpg_decrypt_failed")
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
        print(json.dumps({"verified": False, **safe_error(error)}))
        raise SystemExit(1)
