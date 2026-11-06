# 统一运营管理后台

后台入口为同源的 `/apps/admin/`，前端位于 `livekit-app/apps/admin/web`。账号、组织、权限、审计与任务使用 Foundation；会议、投屏、远控和直播治理使用 AV 专用管理接口。本地播放器不产生服务端会话。

## 构建与运行

要求 Node.js 24、Go 1.25，以及相邻的 `platform-foundation` 源码。Foundation npm 包尚未发布到公共 registry；构建脚本校验源码锁、构建公开包并打包为本地 tarball，随后安装后台依赖、检查契约、运行单元测试并生成静态页面。

在聚合仓库根目录执行：

```sh
node livekit-app/scripts/build-admin.mjs
```

首次开发或明确审核过 Foundation 变更后，使用 `--update-foundation-lock` 更新前端锁并审查差异。日常构建默认只校验，不接受隐式漂移。后端源码锁独立维护：

```sh
cd livekit-server
python tools/foundation_lock.py --check
go build -o out/business-api.exe ./cmd/business-api
```

本机运行在现有服务环境中增加：

```powershell
$env:AV_ADMIN_WEB = (Resolve-Path ../livekit-app/apps/admin/web/dist).Path
./out/business-api.exe migrate
./out/business-api.exe serve
```

首次空账号库按运行手册执行 `bootstrap` 创建管理员；已有账号库不重复初始化。更新已有 AV v2 数据库先备份，再运行追加迁移；服务启动只检查迁移，不自动修改表结构。

`deploy/compose.yaml` 的 API 镜像包含后台静态文件，并设置 `AV_ADMIN_WEB=/opt/av/admin`。构建需要 Foundation 根源码和 livekit-app 两个命名上下文；均由 Compose 使用相对路径提供。部署后访问配置的公开地址下 `/apps/admin/`。反向代理同时转发 `/api/` 与 `/apps/admin/`，`PF_PUBLIC_ORIGIN` 必须与浏览器地址一致。

容器构建缓存 npm 下载。受网络环境影响时，可通过 `GOPROXY`、`GOSUMDB` 和 `NPM_CONFIG_REGISTRY` 构建参数指定可用源；npm 包完整性、Go 校验和与 Foundation 来源锁仍须通过校验。公开包构建会统一文件权限和换行，避免 Windows / Linux 归档差异。

后台静态挂载独立于四业务开关。未设置 `AV_ADMIN_WEB` 时不挂载页面，能力接口不提供可用后台地址。不要用独立域名直接加载 API 而绕开现有 Cookie 与同源校验。

## 管理范围

- 平台管理员默认查看全部组织概况，可以选择任意组织治理，包括尚未加入的组织。
- 组织管理员和具有 `av.manage` 的运营人员仅管理所属组织。成员、角色、任务重试等通用操作仍检查各自权限。
- 全平台账号管理和媒体节点页面仅平台管理员可见。
- 管理接口只接受登录用户会话，拒绝访客资源凭据与 API Key；界面隐藏不是权限边界。

Foundation 的 `/api/v1/admin/organizations/{id}/...` 只用于平台管理员的显式治理。普通客户端的组织成员校验保持不变；平台管理员不能凭普通组织请求头绕过成员关系。切换管理范围会取消请求、清空缓存、关闭旧详情，避免显示上一个组织的数据。

## 页面与操作

| 页面 | 行为 |
| --- | --- |
| 运营概览 | 当前业务状态计数、近 24 小时新增、媒体操作状态；数据来自业务记录，不等同于实时在线人数 |
| 会议 | 预约及会议列表、成员、取消预约、结束会议、静音、邀请开麦、移除非主持成员 |
| 投屏 | 发送／接收状态、会话历史、强制结束 |
| 远控 | 配对与控制授权状态、撤销当前控制、结束会话 |
| 直播 | 场次与连麦状态、关播、聊天禁言／解除禁言、移出非主播成员、嘉宾下麦 |
| 媒体操作 | 业务变更后的媒体清理进度、关联任务、授权重试 |
| 媒体节点 | 健康、启动代次、配置容量、房间绑定数、最近检查时间；过期数据标记未知 |
| 通用管理 | Foundation 账号、组织、成员、角色、审计、任务、事件及运行状态 |

治理操作需要原因。每次确认使用稳定幂等键；结果未知时重试同一操作不会重复生成清理任务。过期版本冲突需要刷新详情后重新确认。

“业务状态已更新”表示授权与状态已经提交；“媒体清理中”表示 Worker 尚在执行移除参与者或删房。只有操作进入成功或已被新状态替代的终态才完成清理。失败操作保留记录，可按权限查看关联任务并重试，不应直接修改数据库或删除幂等记录。

远控撤销保留配对关系，恢复控制必须重新获得被控端本地同意。管理员不能代替用户批准、续租或注入输入。会议邀请开麦不强制打开设备。主持人、主播不能通过普通成员移除操作踢出；治理时使用结束会议或强制关播。直播关播同时处理现有 PK 关系。

节点配置仍由部署文件维护，后台不提供密钥查看、在线修改或启停。管理详情排除媒体 Token、配对凭据、会议密码材料及节点内部密钥。

## 接口与数据升级

AV 管理接口分为：

- `/api/v1/av/admin/platform/`：平台概览、跨组织资源与操作列表、节点监控。
- `/api/v1/av/admin/organizations/{id}/`：指定组织概览、资源列表／详情、媒体操作。
- `POST .../resources/{id}/actions`：带原因的治理操作。

具体参数和类型以合并 `api/openapi.json` 为准。列表默认 20 条、最大 100 条，支持业务、状态、发起人、关键词和时间筛选。已关闭模块的历史记录保留，仍可处理遗留资源的终止和清理。

管理记录、业务状态、审计和异步任务在事务内提交；管理记录保存真实操作者与原因，媒体操作关联 Foundation 任务 ID。新增结构采用追加迁移，既有迁移内容和校验和不变。

## 自动化验证

本次结果与验证边界见[管理后台验证记录](verification-admin.md)。

后台独立检查：

```sh
cd livekit-app/apps/admin/web
npm run verify
npx playwright install chromium --only-shell
npm run test:e2e
```

后端使用独立测试 PostgreSQL 和 Redis，按运行手册设置 `AV_TEST_PG_DSN`、`PF_TEST_PG_DSN`、`AV_TEST_REDIS_URL`，执行后端验证脚本。完整治理联调在聚合仓库根目录执行：

```sh
# AV_TEST_REDIS_URL 指向独立 Redis 测试实例；需要时通过 LK_CHROME 和
# AV_CHROME_PATH 指定已安装的 Chromium headless shell。
python livekit-server/tests/control_plane_integration.py \
  --admin-governance \
  --admin-web livekit-app/apps/admin/web/dist \
  --admin-ui-script livekit-app/apps/admin/web/tests/real-service.mjs \
  --media-script livekit-cpp/tests/av_media_control_smoke.mjs \
  --output livekit-server/out/admin-integration.json
```

该编排使用临时数据库、两个 API 进程、独立 Worker、真实 SFU、独立 Redis 测试服务和无窗口浏览器。平台管理员不加入受管组织或音视频会话，治理通过专用接口执行；媒体来源为合成画面与声音，不采集桌面、不操作系统输入。结果区分接口、媒体传输、界面自动化和实际设备人工验收。
