# 运行与验证

## 源码与构建

要求 Go 1.25。Foundation尚未发布正式模块，开发 workspace 和相对 replace 指向 `../../platform-foundation/platform-foundation-server`；不要改成机器绝对路径。交付应一起保存 Foundation 源码快照和 `foundation.lock.json`，使用 `python tools/foundation_lock.py --check` 检查漂移。

```powershell
go test ./...
go vet ./...
go build -o out/business-api.exe ./cmd/business-api
```

参考 `.env.example` 设置进程环境，数据库必须是新库；程序不会读取 `.env`。先配置 `AV_MEDIA_NODES_FILE`，对应SFU启用AV模式和独立RTC/管理/准入密钥。密钥只保存在可信服务环境。

```powershell
./out/business-api.exe migrate
./out/business-api.exe bootstrap
./out/business-api.exe serve
```

`bootstrap` 只接受空账号库，通过 `PF_ADMIN_NAME/PF_ADMIN_PASSWORD/AV_ORGANIZATION_NAME` 建立管理员与默认组织。启动服务前从环境移除初始化口令。新增组织通过Foundation API创建后，组织管理员调用 `POST /api/v1/av/organizations/initialize {}`，邀请成员时选择 `av-member` 或 `av-operator`。

浏览器同源部署，所有变更请求要求 `X-Foundation-Request: 1`；设置准确 `PF_PUBLIC_ORIGIN`。原生登录提取 `pf_session` Cookie后作为Bearer使用。资源凭证放 `X-AV-Resource-Credential`，避免与用户登录令牌混用。未配置可信代理清单，限流采用直连地址，不信任任意转发IP。

## 容器与扩容

复制 `deploy/.env.example`、`deploy/media-nodes.example.json` 为不提交的实际配置，设置随机口令与可达媒体地址。

```sh
docker compose --env-file deploy/.env -f deploy/compose.yaml up -d --build
docker compose --env-file deploy/.env -f deploy/compose.yaml run --rm -e PF_ADMIN_NAME -e PF_ADMIN_PASSWORD -e AV_ORGANIZATION_NAME api bootstrap
```

Compose的API默认仅绑定本机18080，生产在HTTPS代理后发布。SFU和TURN独立部署，按客户端可达地址及实际网络配置ICE。增加Worker副本可并发领取任务；API多副本应移除单实例固定宿主端口映射，由负载均衡统一接入。业务节点共享PG/Redis和同一配置，不使用粘性会话保证正确性。

`/healthz` 检查进程，`/readyz` 检查PG/Redis，`/api/v1/av/metrics` 需要业务治理权限。关注请求失败率、队列积压/失败、数据库事务等待、媒体准入拒绝及节点状态。日志不包含票据、请求正文或原始媒体Token。

## 故障处理

- API重启：重新接收请求，持久状态不清空，WebSocket重新取票据并补拉。
- Worker重启：队列租约到期后可被其他副本领取。外部操作ID保持稳定，SFU执行版本检查。
- SFU重启：启动代次变化，旧授权拒绝；客户端重新取得新绑定，远控重新本地同意。
- Redis故障：拒绝新票据/续租，远控由本地截止时间停止；会议/直播保存业务事实，恢复后对账。
- PG故障：拒绝授权签发和准入，禁止从延迟副本放行撤权后的请求。
- 任务耗尽：保留failed状态；先确认当前资源和SFU状态，再通过有权限的Foundation任务接口retry。不要删除幂等记录盲目重放未知副作用。

新数据结构不兼容旧服务；升级使用独立新库与同步客户端/SFU发布。后续数据库升级必须保留迁移校验和，先迁移后启动。回滚使用匹配的一组二进制、配置和备份，不让旧业务服务打开新库。

## 自动化检查

```powershell
$env:AV_TEST_PG_DSN = 'postgres://test:password@127.0.0.1:5432/test?sslmode=disable'
$env:AV_TEST_REDIS_URL = 'redis://:password@127.0.0.1:6379/0'
go test ./... -count=1 -timeout 180s
```

PG测试创建独立测试schema；Redis测试使用随机key前缀。无环境变量时相应集成用例明确跳过。真实SFU测试及native/Web构建结果另记验证报告。全部使用编译、HTTP、无窗口测试与合成媒体，不操作桌面；设备人工验收单独记录。
