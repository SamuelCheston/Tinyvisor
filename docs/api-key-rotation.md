# apiKey 泄露处置与轮换步骤

## 背景

`config.json`（含 `apiKey`）曾被 git 跟踪并进入提交历史，等同于泄露：
任何能读到该仓库历史的人都能取得旧 key，可完全控制 Tinyvisor 的全部
API（增删脚本、启停进程、读日志、进终端）。

已执行的处置：

1. `git rm --cached config.json` 并在 `.gitignore` 中忽略——文件仍保留在
   磁盘上供本地实例使用，但不再进入提交。
2. 新增 `config.json.example` 作为无密钥模板。
3. 本检出 `config.json` 中的旧 key 已轮换为新随机值。

**注意：git 历史中仍存在旧 key。** 彻底清除需要重写历史（本次不做）；
因此旧 key 必须视为已泄露，任何曾用它运行过的实例都应完成一次下述轮换。

## 轮换步骤（每个实例）

Tinyvisor 在**启动时**读取 `config.json` 的 `apiKey`，运行中不会重读。
因此轮换 = 改文件 + 重启实例：

1. 确认当前没有依赖旧 key 的自动化正在调用 API（编排器、脚本）。
   若有，先准备好新 key 的注入方式，避免重启后调用方断连。
2. 生成新 key：`openssl rand -hex 16`。
3. 编辑该实例的 `config.json`，把 `apiKey` 替换为新值。
4. 重启 Tinyvisor 实例（systemd：`systemctl restart tinyvisor`；
   前台进程则停止后重新拉起）。
5. 用新 key 验证：
   `curl -H "X-Minivisor-Key: <新key>" http://127.0.0.1:<port>/api/scripts`
   应返回 200；再用旧 key 请求应返回 401。
6. 把新 key 更新到所有调用方（例如 HA-Services-Deployer 的本地凭据文件）。

未重启的实例会继续用旧 key 工作，不会中断；但旧 key 已泄露，
应在方便时尽快按上述步骤完成重启切换。

## 预防

- `config.json` 永远不进版本库（已 gitignore）。
- 新部署从 `config.json.example` 复制并本地生成 `apiKey`；
  留空时首次启动也会自动生成随机 key。
- 远程配对走一次性 PIN（见 `docs/pairing_pin_design.md`），
  不要通过聊天/文档传递长期 key。
