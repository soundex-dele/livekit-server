# livekit-server

AV Platform 的业务服务仓库。

当前仅初始化仓库，尚未实现服务功能；技术栈、构建方式和运行入口待确定。

## 规划职责

- 账号、设备关系、会议与远控授权。
- 校验业务权限并签发短期媒体访问 Token。
- 媒体资产、计费与相关业务状态管理。
- 通过服务端 SDK 或管理 API 协调媒体服务。

媒体服务 API Secret 仅保存在可信后端，不下发给客户端。
本仓库负责业务服务；SFU 转发、TURN、录制和转码等媒体服务能力由 `livekit-cpp` 及相关服务承担。

## 关联仓库

- [av-platform](https://github.com/soundex-dele/av-platform)：聚合仓库与平台架构文档。
- [livekit-app](https://github.com/soundex-dele/livekit-app)：客户端应用。
- [livekit-sdk-cpp](https://github.com/soundex-dele/livekit-sdk-cpp)：现有原生 C++ RTC SDK 及服务端 API 客户端基础。
- [livekit-cpp](https://github.com/soundex-dele/livekit-cpp)：媒体服务。
