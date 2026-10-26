# livekit-server 业务服务设计

实现补充：首版远控业务服务采用 Go + SQLite，后续通过仓储接口迁移 PostgreSQL；使用临时配对，不包含账号体系。已实现部分及验证边界见 [远控运行文档](../../docs/remote-control/README.md)，下文保留完整平台的目标设计。

版本：v0.1；日期：2026-10-26；状态：目标设计，仓库当前仅有初始化文件。

本仓库提供 AV Platform 业务控制面。媒体服务在 `livekit-cpp`，其当前二进制也叫 `livekit-server`；本仓库未来进程/镜像建议命名 `livekit-business-api`，避免部署歧义。跨仓库契约以 [总体设计](../../docs/platform-design.md)为准。

## 1. 目标与职责

管理用户、项目成员、设备、会议权限、远控授权、媒体任务、资产与审计；向已授权客户端签发短期媒体 Token；通过受信管理 API 协调媒体实例。

本服务不接收用户 RTP/SRTP 媒体，不实现编解码和 SFU，不把媒体在线状态直接持久化为权威业务事实。账号认证与媒体 Token 分开；媒体服务 API Secret 仅保存在可信后端。

首期只实现账号接入、项目成员、会议、入会 Token 与审计最小闭环。任务、资产、计费和多节点调度是后续目标。

## 2. 推荐技术与模块

建议 Go + PostgreSQL，采用模块化单体，按领域拆包、统一事务，不首期拆成微服务。Go 仅为设计建议，确认前不生成框架代码。对接媒体侧可使用固定版本官方 Server SDK，但必须通过当前 C++ 服务支持的 RoomService 子集测试；有客户端方法并不意味着目标媒体服务支持。

```text
cmd/business-api/       进程入口和依赖装配
internal/identity/     登录认证、会话与成员
internal/projects/     项目、角色、配额
internal/devices/      设备登记、归属和在线租约
internal/meetings/     会议生命周期、入会策略
internal/grants/       远控申请、同意、撤销和租约
internal/media/        媒体管理适配、Token、能力与实例选择
internal/jobs/         持久化任务、尝试、租约与状态转换
internal/assets/       资产元数据、可用性与下载授权
internal/audit/        审计与脱敏
internal/outbox/       事务事件投递和重试
api/                   OpenAPI 与版本化契约
migrations/            数据库迁移
tests/                 契约、数据库、集成与故障测试
```

请求层完成解析和认证，领域服务执行授权/状态规则，仓储处理数据库，media 适配器封装外部副作用。领域事务不持有数据库锁等待长时间网络请求。

## 3. 数据模型（拟定）

| 表/聚合 | 关键字段 | 约束 |
| --- | --- | --- |
| users / auth_sessions | user_id、认证主体、会话状态、到期时间 | 首期优先接入 OIDC；若自管密码需单独设计哈希、恢复与风控 |
| projects / memberships | project_id、user_id、role、status | 成员唯一键 `(project_id,user_id)` |
| devices | device_id、project_id、owner_id、last_seen、credential_ref | 设备凭证可撤销，不信任客户端自报归属 |
| meetings | meeting_id、project_id、media_room、media_instance、state、version | 房间映射唯一；业务版本用于并发更新 |
| meeting_members | meeting_id、user_id、role、policy | 业务角色不同于当前媒体是否在线 |
| control_grants | grant_id、双方身份/设备、meeting_id、scope、epoch、state、expires_at | 被控设备审批；状态转换校验版本 |
| media_jobs / job_attempts | job_id、project_id、type、state、attempt、lease_owner、lease_until、fencing_token | 同一任务只有一个有效租约 |
| assets | asset_id、project_id、job_id、object_key、checksum、state | available 前完成上传与校验 |
| idempotency_records | actor、route、key、request_hash、result_ref、expires_at | 唯一作用域；不同内容同键冲突 |
| outbox / inbox | event_id、payload、attempt、next_attempt_at | outbox 与业务同事务；inbox 去重 |
| audit_events | actor、action、resource、decision、request_id、time | 不保存密钥、完整 Token 或媒体内容 |

所有领域查询必须包含项目归属验证；数据库唯一键和外键尽可能包含 project_id 防止跨项目关联。设备心跳和媒体观察值有时间戳与 TTL，超过窗口标为未知/离线，不无限保留在线状态。

## 4. API 与身份授权

接口统一 `/api/v1`、JSON snake_case、UTC RFC3339；路径与错误包络沿用总体设计。实现时在 `api/` 输出 OpenAPI，业务与客户端以同一契约生成或校验类型。

认证层得到 user_id 和应用会话；授权层根据 membership、对象归属、会议策略和请求能力计算允许集合。普通客户端不能指定任意媒体 room、identity、issuer 或 management grant。媒体实例由服务端映射，响应只返回经过配置的客户端可达地址。

首期建议 owner/admin/member/viewer 四类项目角色，会议主持/参会角色独立映射；具体权限以能力集合表达，避免把所有 admin 权限直接映射为媒体 roomAdmin。每个管理操作均生成审计决策。

Token 服务生成短期、最小权限、单房间 Token。TTL、可接受时钟偏差由配置提供；部署必须监测时钟偏差。测试覆盖 exp/nbf、错误 issuer、错误房间、禁止发布/订阅/数据与未经支持的权限限制。

应用会话撤销和媒体连接终止是两个动作。退出登录/封禁后阻止新 Token，按策略移除当前参与者；媒体 Token 到期不被当作已连接会话的实时撤销机制。密钥轮换需要业务与媒体服务双侧协议支持，未实现前不宣称无损轮换。

## 5. 会议生命周期与跨服务一致性

拟定状态为 preparing → active → ending → ended；准备失败可进入 failed，结束过程保留可重试的清理记录。会议进入 ending 后即停止签发 Token，无需等待媒体删除成功。

创建流程：在数据库按幂等键登记 preparing 会议及稳定 media_room → 通过媒体 API 确保房间存在 → 更新 active。超时后先查询确认，不改名重复创建。失败通过记录重试或补偿清理，禁止在 HTTP 返回失败后遗留不可追踪的房间。

入会流程：检查项目/会议/设备/容量策略 → 读取媒体实例能力 → 计算授予权限 → 签发 Token → 审计。容量可能在 Token 签发后变化，最终媒体服务仍需执行房间容量限制，客户端正确处理拒绝。

结束流程：数据库标记 ending 并写 Outbox → 删除媒体房间/移除成员 → 对账确认 → ended。业务已终止但媒体清理暂失败时返回明确 cleanup_pending 状态，后台继续收敛并告警，不能伪报即时完成。

当前媒体服务无 Webhook，采用有界频率的管理 API 对账；future Webhook 通过验签、时窗、inbox 去重和对象版本更新状态。过期事件不能复活 ended 会议。

## 6. 远控授权

授权状态为 pending/approved/revoked/expired/rejected；申请绑定控制者、被控设备、会议和请求 scope。只有经认证的目标设备及其本地同意流程可以批准，申请者不能自批。授权使用短期租约与 epoch，续租再次验证成员、会议和本地同意状态。

批准后使用可验证的专用授权凭证或在线确认结果传给被控 SDK，具体签名格式在实现阶段固定；不得复用宽泛的媒体 Token 充当输入授权。被控端核对控制者媒体 identity、会话代次、序号与屏幕布局版本。

撤销先更新数据库并通知双方，被控端立即释放输入。通知不达时由租约到期阻止继续操作；产品必须公开这一撤权边界。断线重连后重新授权，不自动恢复已失效控制权。

## 7. 任务、资产与可靠执行

任务 API 返回 202 与 job_id，表示已持久化接受。worker 协议属于内部认证接口，拟定 acquire/heartbeat/progress/complete/fail/cancel 操作，每次携带 job_id、attempt、fencing_token。执行模块由 `livekit-cpp` 的未来独立 worker 提供，业务服务不实现媒体处理。

数据库条件更新取得租约；续租失败的 worker 停止执行或进入不能提交结果的状态。租约到期可调度新 attempt，旧 token 的 progress/complete 被拒绝。至少一次投递意味着 worker 必须去重，外部推流等不可简单重复的任务按类型制定恢复策略。

上传使用 attempt 专属临时 object_key；校验后业务事务登记产物并置 succeeded。相同 attempt 重复完成返回既有结果；失败临时文件按策略清理。取消请求将状态改为 cancel_requested，收到执行端确认再置 cancelled；租约丢失后的恢复过程仍需处理残留 worker。

资产下载先检查项目和用户权限，再签发短期下载地址；对象存储默认私有。删除资产先撤销业务可见性，再异步删除对象并对账。配额至少覆盖并发任务、存储、会议数，计量与结算后续实现，不由估算值直接扣费。

## 8. 安全、运行与故障处理

HTTP 配置请求大小、超时、主体/项目速率限制和可信代理边界；媒体管理接口只在受控网络使用，认证仍需保留。Secret 从部署密钥源注入，不写入仓库、日志或返回包络。数据库迁移和服务启动使用不同权限账号是部署目标。

拟定 `/healthz` 检查进程存活，`/readyz` 检查数据库与配置可用性；媒体实例故障应返回受影响能力 unavailable，避免把所有只读业务请求一起置为不可用。优雅退出停止接收新任务与请求，等待有限时间并释放调度租约。

记录 API 延迟/错误率、鉴权拒绝、Token 签发失败、媒体对账滞后、Outbox 积压、任务租约丢失、资产校验失败。日志携带 request_id 与资源标识，指标避免高基数。备份包含数据库和资产清单，恢复后重新对账媒体瞬态状态。

## 9. 实施顺序与验收

1. 确认语言、OIDC/自管认证方案和数据库；建立 OpenAPI、迁移与配置规范。
2. 实现项目成员、会议、Token、RoomService 适配与审计；完成双用户入会闭环。
3. 增加设备与远控授权、撤权及租约，覆盖通知丢失和重连。
4. 增加任务、Outbox、worker 契约、资产和幂等恢复。
5. 根据容量证据增加控制面多副本和媒体实例调度。

必要测试覆盖跨项目 ID 替换、角色越权、同键重试/冲突、并发结束与入会、媒体请求超时但成功、数据库故障、时钟偏差、重复/乱序事件、worker 崩溃和迟到结果。真实媒体集成需验证实际解码与离会清理，不只验证 Token 字符串生成。当前未实现上述服务或测试。
