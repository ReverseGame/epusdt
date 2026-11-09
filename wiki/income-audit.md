# USDT-TRC 独立收入核对

核对任务默认关闭，不创建付款记录，也不更改客户订单支付状态。每次支付监听触发时，在支付锁外启动独立 goroutine；进程内最多一个核对任务。

## 启用配置

1. 在 epusdt 数据库执行 `sql/20261109-incoming-transfer-event.sql`。
2. 在部署使用的 `.env` 中补齐 `src/.env.example` 的 `income_audit_*` 配置。当前应用通过 Viper 读取 `.env`，不要仅设置容器环境变量而遗漏文件配置。
3. 起始时间是 Unix 毫秒；钱包是本业务收款钱包白名单，以英文逗号分隔。扫描窗口默认一小时，达到一万条分页限制不会推进游标，需调小窗口后重跑。
4. TronScan 和 TronGrid 的 API key 分别用于索引与已固化回执查询。
5. 通知地址必须为 HTTPS，路径 `/internal/payment/unmatched-income`。接收端同时配置 `income-anomaly.enabled=true`、相同的钱包白名单及至少32字符的共享密钥。密钥仅通过环境配置系统管理，不提交仓库。
6. 确认接收端通知队列及业务 @ 配置正常，再设置 `income_audit_enabled=true`。Viper 支持 `true/false`、`1/0`。

## 分页、匹配与投递

分页依据 `len(data)`：小于50为末页，等于50继续，空数组或null为无记录；非200、超过50条、回执无效等错误不推进游标。`page_size` 不再作为完整性断言。官方当前将该字段描述为本页数量，因此不能据此断言线上接口一直返回总数：<https://docs.tronscan.org/en/api/wallet/transfer-trc20>。

同一交易按 `tx_hash + wallet` 分组，读取该组全部事件，不受单批50组限制截断。金额使用十进制规范化，并从数据库以文本读取订单金额；只有事件与已付款订单双向唯一时才视为匹配。

沿用逐事件接收协议：每个事件使用稳定 `eventKey=tron:<txHash>:<eventIndex>`。该组全部异常事件都被接收端受理后，才在一个数据库事务中提交本组 MATCHED/NOTIFIED 状态。任何一次失败，全组待处理事件统一退避重试，先前已受理事件依靠接收端唯一键去重。跨服务没有分布式事务，接收端可能暂时先看到部分事件；本地不会将半组提前标为 NOTIFIED。

签名头为 `X-Income-Signature: <Unix秒>:<小写hex>`，计算 `HMAC-SHA256(secret, timestamp + "." + 原始JSON正文)`。接收端已有前后300秒时间窗、恒定时间比较和事件唯一键，只以 HTTP200 且 `accepted:true` 作为持久化成功。失败不改变原付款匹配流程。

## 失败与人工恢复

整组投递失败最多8轮，退避30、60、120、240、480、960、1920秒，随后状态 FAILED 停止自动投递；reason 保存业务原因与最后错误。查询失败记录：

```sql
SELECT tx_hash,wallet,state,attempts,reason,next_attempt_at
FROM incoming_transfer_event WHERE state='FAILED' ORDER BY id;
```

先修正接收地址、签名密钥、白名单或接收端服务，再按已核实的交易和钱包重置整组；原始 event_key 必须保留，避免重复通知。不要将支付订单改为已付款：

```sql
UPDATE incoming_transfer_event
SET state='PENDING',attempts=0,next_attempt_at=NOW(3)
WHERE tx_hash='<已核实的交易哈希>' AND wallet='<已核实的钱包>'
  AND state IN ('FAILED','PENDING');
```

多个进程之间没有新增分布式互斥；启用核对时维持单副本核对进程。数据库唯一键与接收端事件幂等可处理崩溃后的重复，但不作为多副本调度保证。
