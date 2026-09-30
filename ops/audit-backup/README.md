# 业务数据库备份

这套运维程序与 Kilo 转发服务互不调用。源码借用已有仓库及 CircleCI 存放与验证，部署不会替换或重启业务程序。

## 已核实范围

119 的订单/卡密 `redeem`、`dujiao` 两个 SQLite，99code 的唯一业务 MySQL 库 `newapi_523`，Tag、未完及 Monitor SQLite、mailflow 的 `mailflow` PostgreSQL，以及小恩爱已有最近 MySQL 备份。SQLite 复制全部关联表，仅在还原副本上对主要表计数；不裸复制 WAL 活库，不查询 3000 NewAPI MySQL。

Frequency 没有找到正在运行的服务或活库，保留明确缺口，绝不用空库冒充覆盖。小恩爱只接受 48 小时内且 checksum、gzip、dump 完整性检查通过的现有备份，不再发起第二次 dump。

`targets.json` 明确来源与 `50:/opt/backup-archive/node119/audit-core` 目的地。本地与远端都是 root-only。沿用现有 SSH，检查已知主机密钥，不关闭 host-key 验证，不添加云服务或外部凭据。

## 执行与验证

- `python3 backup.py`：默认只读预览。
- `python3 backup.py --apply --replicate`：逐项串行备份、压缩、SHA-256、原子发布，复制异机并独立检查。
- `--targets redeem,dujiao,99code`：只运行所列精确目标；manifest 明确范围，不会宣称全覆盖。
- `--full-check`：将 SQLite 的首次完整性验收改为 `integrity_check`；日常为 `quick_check`。在线 backup 有页批量、忙等待及总时限，仍可能有短时 I/O 与读写争用。
- `python3 verify.py --directory /opt/backups/audit-core/<run-id>`：在新的临时文件中解压、只读检查 SQLite 与主要表计数，检查其他 dump 的完整性，源库不动。

SQLite 的两节点验收包括真正解压成独立数据库再只读打开。MySQL 检查 gzip CRC、建表定义、dump 结束标志；Postgres 在来源端检查 `pg_restore --list`，异机检查压缩完整性和归档头。这两类**尚未恢复入隔离数据库**，不能声称完整业务恢复演练已通过。隔离恢复需要另核容器镜像、磁盘/内存预算，禁止连接生产库或公开端口。

单项失败写独立结果，成功项仍复制保存；任何失败都会返回非零，且不删除旧备份。MySQL 与 PG 的秘密只在容器内部给客户端使用，不进入命令参数、manifest 或日志。数据库 dump 的 stderr 不输出。MySQL 仅使用本次容器内客户端 PID 对应的连接做退出核验/取消，不能按库名或 root 用户批量杀连接；PG 仅按本次唯一 application_name 取消。

## 日常与保留

CI 产物验证后，先在 50 运行 `install.sh --apply --archive`，再在 119 运行 `install.sh --apply`。默认不启用 timer；首次全部备份/异机验收成功后再用 `--enable-timer`，或单独 enable 对应 timer。每日 UTC 04:40，加最多 5 分钟随机延迟；程序锁防止自己的定时与手工任务重叠。

安装要求 CI 的 `source-commit.txt` 与 `AUDIT_CORE_BACKUP_CONFIRM_COMMIT` 完全相符。CI 文件解压应使用 `--no-same-owner`，放进主代理已核实 SHA/commit 的 root-only staging，再将已验证 `source-commit.txt` 放入各模块目录；来源/目的文件必须 root-owned、普通单链接，目录不可经过符号链接。安装前记录新任务 timer 的启用/运行状态及原文件，不把状态回退与数据库恢复混为一谈。

备份各项串行；要求至少 10GiB 剩余。主要活库估计：SQLite 最大 2.3GB、MySQL 数据索引约 2.2GB、Monitor 1.3GB、小恩爱压缩包约 294MB。备份、压缩与恢复检查会读写数 GB，不能承诺绝对零干扰，建议低谷执行。单项 SQLite 最大 600 秒、MySQL 900 秒、PG 180 秒；内层数据库客户端有 timeout，外层再次限时；systemd 的整批时限 2 小时、低 CPU/IO 优先级。

成功异机副本保存后，本地和异机只清理这套程序创建且超过 7 天、checksum 验收成功、当前新包范围覆盖的旧 run。既有手工快照、其他目录、源库、失败或没有异机验收证据的包均保留。不认识的文件、链接或残留临时目录都会阻止删除。失败造成的未验收包可能超过 7 天，需查看失败原因及剩余空间，不盲目删除。

回退是停用这套新 timer 并还原安装前文件，不重启业务，也不自动删除已经产生的备份。安装文件的 root-only 副本在 `/opt/audit-core-backup-install-backups/`。任务新增 timer 不触碰 Monitor 全局维护、SSH、防火墙或业务服务配置。
