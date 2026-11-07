# 编排器对接接口（HA-Services-Deployer 消费）

Tinyvisor 作为目标机进程管理器，被 HA-Services-Deployer 的裸金属编排能力
驱动。本文档记录编排器依赖的接口约定（2026-11-08 增补）。

## 稳定句柄：脚本名唯一

- 创建/更新脚本时强制 `name` 唯一，重名返回 `409 {"error":"脚本名称已存在"}`。
- 编排器一律以 `name`（= 项目 ID）寻址，不依赖内部生成的 `script-*` ID。

## 按名端点

| 端点 | 语义 |
|---|---|
| `GET /api/scripts/by-name/:name` | 解析脚本；不存在 404 |
| `PUT /api/scripts/by-name/:name` | 幂等 upsert：不存在创建（201, `created:true`），存在更新（200, `created:false`）；路径中的 name 覆盖请求体 |
| `POST /api/scripts/by-name/:name/restart` | 原子重启（见下） |
| `POST /api/scripts/:id/restart` | 同上（按 ID） |

鉴权与其它端点一致：`X-Minivisor-Key` 头（或 `?key=`）。

## 原子重启

`restart` = 运行中则停止（等待退出，超时强制结束）后启动；未运行则直接启动。
单次调用完成，避免 stop/start 分两次调用时与自动重启观察窗竞态。

## 编排器侧的脚本约定

- 脚本内容必须以 `exec <二进制>` 结尾：Tinyvisor 的 SIGTERM 发给 bash 进程，
  `exec` 后服务进程取代 bash，才能直接收到停止信号。
- `workDir` 为部署根目录；脚本以 `exec ./current/<二进制>` 方式指向生效版本软链。
- 编排器按 `restartPolicy`/`autoStart` 声明登记；内容无漂移时不重写脚本。

## 健康分工

Tinyvisor 只管进程存活（PTY 会话在否）与按策略重启；应用级健康探测
（HTTP 探针）由编排器按各项目 Manifest 执行，不在 Tinyvisor 内实现。
