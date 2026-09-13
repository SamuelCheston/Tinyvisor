# Pairing PIN 即用即弃与本地生成策略设计

## 1. 背景与目标
当前 `PairingPIN` 缺省在启动时自动生成，且在成功配对后仍然保留，形成长期可用凭据窗口。  
目标：
- 配对 PIN 一旦被前端成功使用，立即失效（Use-and-Burn）。
- 未收到“生成新 PIN”请求前，保持缺省不可用状态（无可用 PIN）。
- 新 PIN 生成方式限定为本地 TUI 热键触发，避免远程滥发生成请求。
- 首次部署生成的 PIN 无时间限制；后续手动生成的 PIN 强制 **10 分钟**自然过期。

## 2. 决策结论（已对齐你的指令）
| 决策项 | 结论 |
|---|---|
| PIN 生成触发方式 | 本地 TUI 触发（方案 A） |
| 缺省未生成时状态 | 默认显示为“未生成/已过期”，不可配对 |
| 时间限制策略 | 首次生成不限时；后续生成 10 分钟过期 |
| PIN 使用后处理 | 立即清空内存与持久化值，并重置过期计时器 |
| API 暴露 | 不暴露远程 PIN 生成接口 |

## 3. 方案总体设计

### 3.1 状态模型
新增两个运行时状态字段（内存为主，持久化为辅）：
- `pairingPIN`（string）：当前有效 PIN 或空字符串。
- `pinCreatedAt`（time.Time）：当前 PIN 生成时间（用于判断是否为首次生成）。
- `pinExpiresAt`（*time.Time）：当前 PIN 过期时间（`nil` 表示不限时，仅首次可为 `nil`）。

语义：
- 若 `pairingPIN == ""`：当前不可配对。
- 若 `pinExpiresAt != nil && now.After(pinExpiresAt)`：视为过期，等同于不可配对。
- 若 `pinExpiresAt == nil`：仅在“首次生成”场景允许。

### 3.2 首次生成 vs 后续生成
定义“首次生成”判定规则（二选一，推荐 A）：
- **A（推荐）**：若 `config.json` 当前不存在文件，且本次是程序自动生成初始配置，则标记为首次。
- **B（备选）**：新增配置字段 `pinGenerationEpoch`（计数器），`0` 视为首次。

推荐方案 A，因为与现有 `setupEnvironment()` 的首次初始化逻辑一致，改动最小。

## 4. 接口与交互设计

### 4.1 `/api/pair` 行为变更
请求：
```json
POST /api/pair
{
  "pin": "1234"
}
```
执行顺序：
1. 校验输入。
2. 检查当前是否具备可用 PIN：
   - `pairingPIN == ""` 或已过期 -> 返回 409/403（建议 409，语义“当前无有效配对码”）。
3. 比对 `payload.pin == config.PairingPIN`。
4. 一旦匹配成功：
   - 返回 `apiKey`。
   - **立即清除** `pairingPIN`、`pinExpiresAt`、`pinCreatedAt`。
   - 持久化到 `config.json`（清除 `pairingPIN`）。
   - TUI 立即刷新状态提示。

### 4.2 TUI 交互
新增操作（仅本地）：
- 热键 `G`：请求生成新 Pairing PIN。
- 生成逻辑：
  1. 调用 `app.generatePairingPIN()`。
  2. 若属于首次生成：设置 `expiresAt = nil`。
  3. 否则设置 `expiresAt = now + 10m`。
  4. 持久化 `pairingPIN`；对 `expiresAt` 建议持久化，保证重启后仍遵守 10 分钟限制（避免通过重启延长有效期）。
  5. TUI 输出新 PIN 与剩余有效时间提示。

TUI 显示逻辑：
- 无可用 PIN：`Pairing PIN: [未生成/已过期]`
- 有效且不限时：`Pairing PIN: 1234 (无时限)`
- 有效且有时限：`Pairing PIN: 1234 (剩余 9m42s)`

### 4.3 `/api/config` 是否返回 PIN 时长信息（可选）
不建议在对外 `/api/config` 返回具体 PIN。但可返回布尔状态字段（仅用于 UI 状态展示，不影响安全）：
```json
GET /api/config
{
  "port": 18083,
  "name": "Tinyvisor Single Binary",
  "hasPairingPIN": false,
  "pinIsTimeLimited": false
}
```
说明：该字段仅提示“当前是否有有效 PIN”，避免泄露 PIN 本身。

## 5. 存储与持久化设计

### 5.1 `config.json` 字段调整
保留 `pairingPIN` 为空字符串表示当前无效。  
新增字段：
- `pinCreatedAt`（string, RFC3339，可为空）
- `pinExpiresAt`（string, RFC3339，可为空）

示例：
```json
{
  "port": 18083,
  "name": "Tinyvisor Single Binary",
  "apiKey": "...",
  "pairingPIN": "",
  "pinCreatedAt": "",
  "pinExpiresAt": ""
}
```

### 5.2 过期判定与清理
- `/api/pair` 请求时先做“懒过期检查”。
- TUI 刷新循环中每 1 秒做状态显示更新。
- 后台无需单独定时清理，只需要判定逻辑正确即可。

## 6. 安全设计要点

1. 防止空对空命中：
   - 当 `pairingPIN == ""`，直接拒绝配对，避免输入空字符串误匹配。
2. 防止重启绕过时限：
   - `pinExpiresAt` 必须持久化，重启后继续遵守剩余时间。
3. 限制生成入口：
   - 不暴露 `/api/generate-pin`；所有生成走本地 TUI。
4. 防暴力尝试（可选增强）：
   - 同一 IP 在短时间 N 次失败后临时限流（当前设计先不做，避免复杂度上升）。

## 7. 关键模块改动点（不写代码，仅定位）

- [backend/main.go](file:///home/lnb/Minivisor/backend/main.go)
  - 现有 `/api/pair`、`setupEnvironment()`、启动打印逻辑、`Config` 结构体。
- [backend/tui.go](file:///home/lnb/Minivisor/backend/tui.go)
  - 新增 `G` 热键、状态显示、生成动作调用。
- [config.json](file:///home/lnb/Minivisor/config.json)
  - 增加 `pinCreatedAt`、`pinExpiresAt` 字段示例。
- [frontend/src/App.tsx](file:///home/lnb/Minivisor/frontend/src/App.tsx)
  - 仅需要处理 409/403 的新错误提示文案（若后端返回新错误码），不改配对协议。

## 8. 前端影响评估
- 不改变配对协议主体：仍为 `POST /api/pair { pin } -> { apiKey }`。
- 需要对“当前无有效 PIN”错误做明确提示，例如：
  - “当前配对码未生成或已失效，请在服务器本地按 G 生成新配对码。”

## 9. 验收标准
1. 首次部署生成的 PIN 不限时，成功使用后立即失效，无法再次使用同一 PIN。
2. 后续通过 TUI `G` 生成的 PIN 在 10 分钟内有效，超时后无法配对。
3. 无可用 PIN 时，TUI 明确展示“未生成/已过期”，前端请求返回相应错误。
4. 服务重启不会延长后续生成 PIN 的生命周期。
5. 无远程生成接口暴露。

## 10. 实施顺序建议（后续再动代码）
1. 扩展 Config 字段与持久化逻辑。
2. 改造 `/api/pair` 为 Use-and-Burn + 过期检查。
3. 新增 `App.generatePairingPIN()`（含首次/非首次策略）。
4. TUI 增加热键与状态展示。
5. 前端错误提示适配。
6. 端到端手动验证与回归测试。
