# 加密源码保全

仅运行在 119，复制到 50 上已批准的私有目录。保存清单内的每个项目；不 fetch/push，不改分支，不替用户提交现有脏改动。正式安装使用 CI 验证并下载的同提交源码包。

## 包含与缺口

- 有提交的 Git 仓库：`git bundle --all HEAD` 保全当前 HEAD 和全部可达引用，包括本地分支、标签、远端跟踪引用、stash；不保证保全已经没有任何引用的悬空对象/仅在 reflog 中的旧提交。
- 所有项目：安全工作区，及可保存的 staged/unstaged 二进制差异；没有 Git 或尚无首个提交也可备份。
- 普通工作区不含 `.env`、私钥/凭据、数据库、日志、构建物、嵌套独立仓库、链接/特殊文件。大于 16 MiB 的单文件跳过并计数；过大的差异文件也会计入缺口。嵌套仓库应另列清单，不通过父目录漏备或跨边界读取。
- Caddyfile 只进包内 `private-config/`，不混进普通工作区。历史 Git 对象可能含旧密钥，所以每个项目的整个包都用 GPG AES256 对称加密。备份程序和日志不输出源码、差异、远端 URL、密钥或环境内容。
- 包内记录所有省略路径/原因/计数。`verified=true` 表示“本次声明范围可恢复”，不是所有文件都已保存；业务数据和部署 secret 的恢复由业务备份及独立 secret 保全负责。

## 运行

```bash
python3 backup.py --config sources.json
python3 backup.py --config sources.json --only /root/ai-sites
bash install.sh
```

以上均预演。经主代理核实 CI 同提交源码后才用 `install.sh --apply`，需要 CI 包内 `source-commit.txt` 与环境 `AUDIT_SOURCE_BACKUP_CONFIRM_COMMIT` 精确一致；现有文件和定时器状态先保全到 root-only 安装回退目录。默认不启用 timer，首批烟测可运行 `backup.py --apply --only /root/ai-sites`，验证后安装时加 `--enable-timer`。完整运行由 `audit-source-backup.service` 执行，后续每日 03:40 后随机延迟最多 15 分钟。与业务备份共享 `/run/lock/audit-core-backup.lock`，最多等待 10 分钟；两者不会并发。锁超时使本次任务失败，不会清理旧备份。

本地保留 `/opt/audit-source-backups/<项目ID>/`，异机保留 `50:/opt/backup-archive/node119/audit-source/<项目ID>/`。这是两台服务器上的副本，不代表异地或跨服务商容灾。每项目分别留最近 7 个成功完成异机解密和恢复校验的加密包；失败包不计入成功数，也不触发旧保险删除。两机每次任务前至少 10 GiB 空闲；运行中低于此线会停止当前项目。失败包只可能属于本任务，可在查清失败原因后另行清理。

## 密钥与恢复

程序首次执行生成 root-only `/root/secrets/audit-source-backup.pass`（600），并经 SSH 加密传输复制到 50 同名 root-only 文件。既有异机密钥不匹配时拒绝覆盖、任务失败。真实值只由程序读，不进入输出。两机密钥不能与备份包一起公开；应另做离线保管，否则同时失去两机密钥后加密包无法恢复。

每仓验证包括：源码文件复制前后身份/时间/大小不变、最终安全文件清单与 HEAD/refs/index/status 不漂移，bundle 本地 verify、mirror clone 后全部引用匹配，tar 无链接/路径穿越且各成员尺寸/hash 匹配；50 再核验加密包 SHA256、解密、mirror clone、成员内容hash。中途漂移则该项目失败，旧备份原样。

恢复请先进入 root-only 临时目录，用本机密钥通过 `gpg --batch --pinentry-mode loopback --passphrase-file <密钥路径> --decrypt --output <临时tar> <加密包>` 解密，再用 `verify.verify_archive` 验证并安全解包。`history.bundle` 可恢复完整引用；`worktree/` 是当前安全文件；`changes/` 保留 staged/unstaged 差异；`private-config/` 不应直接覆盖正在运行的配置。先在独立恢复目录应用差异并核对省略清单，禁止覆盖原项目或部署 secret。恢复 Git 时要注意历史对象可能包含旧密钥，不应上传到公开仓库。

## 回退与本地测试

只停用 `audit-source-backup.timer`，让已经开始的备份自然结束；保留加密包及两机密钥，不删除备份、不重启任何应用。若需更换程序，恢复上一份已验证 CI 文件后重启本任务的定时器。

```bash
python3 -m unittest discover -s ops/source-backup -p 'test_*.py' -v
```

这些测试只在临时目录创建仓库，验证边界、脏改动、空仓、秘密排除、漂移、恢复和保留。它们是本地验证，不能代替正式 CI 或真实异机恢复验收。
