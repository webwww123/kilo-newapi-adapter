# 隔离恢复验收（第二独立交付）

`restore_isolated.py` 只接受本任务已成功生成且 manifest/SHA-256 完整的 99code、小恩爱或 mailflow 包。默认只 preview；`--apply` 才创建临时数据库容器。

运行前使用实时 `node-index` 与本次内存、磁盘预算检查。先停止其他大备份/源码归档作业的并发竞争，再串行执行，不重压生产。工具还会确认至少 2GiB 额外可用内存，以及不少于 10GiB/原始包两倍加 2GiB 的磁盘余量；余量不足就拒绝开始。

`python3 restore_isolated.py --run /opt/backups/audit-core/<run-id>` 预览计划。

`python3 restore_isolated.py --run /opt/backups/audit-core/<run-id> --apply` 逐项导入新的隔离库，再核对 schema、表数量及少量主要表行数；只输出计数，不输出用户/卡密/凭据/SQL 原文。

必需表以本次生产元数据核实为准：99code 的 users/tokens/redemptions/top_ups/channels，小恩爱的 User/AccountEntitlement/AccountTokenEntitlementState/PaymentOrder/UserMessage，以及 mailflow 的 users/email_accounts/messages。验收时任何必需表缺失都判失败，不能把零个主要表检查视为恢复通过；查询业务值不属于验收内容。

临时镜像取当前对应源容器 `docker ps` 的 Image 元数据，禁止 inspect/env、禁止拉取新镜像。每个容器 `--network none`、没有任何端口映射、1 CPU；MySQL 1.5GiB 内存，PG 512MiB，swap 等于内存上限。MySQL 关闭 event scheduler 和 binlog，恢复数据不会发邮件或调用上游。密钥在本轮私有目录内随机生成到 0600 文件，传进容器的只是文件路径。

目前资源估算：MySQL 每次临时数据约 2–4GB、CPU/磁盘 I/O 数分钟；小恩爱约 294MB 压缩包，实际膨胀按 manifest 校验；PG 约 10MB 库。不能保证精准耗时或零影响，主代理以执行时余量决定是否开始。

所有写入都发生在本次创建的数据库。正常结束或超时都只按 docker create 返回的完整 CID 停止并移除测试容器；容器真正消失后才删除本轮唯一私有目录。未成功移除时停止后续恢复并报告待处理目录，绝不清生产数据、原始备份或别的容器。报告保存在 `/opt/backups/audit-core/restore-tests/reports/`，不会向归档 run 混入未知文件而妨碍 7 天保留。

本工具不安装定时器、不重复 dump、不改生产 MySQL/PG 配置、不公开端口，也不启用 Monitor 维护模式。正式采用必须走独立 CI 源码包，主代理批准执行资源窗口后才实际运行。
