# loomy-accounts — Loomy provider plugin for CLIProxyAPI

**v1.0.0** · 实测环境：CLIProxyAPI 8.0.53 / linux-arm64 · 2026-11

把 Loomy（讯飞桌面 AI 助手）接入 CPA：一个账号一个 auth 文件，账号池由 CPA 自己的调度器管理，插件的管理面板负责看余额、看请求、看模型。

把 Loomy（讯飞桌面 AI 助手）接入 CPA：一个账号 = 一个 auth 文件，池子由 CPA 自己的调度器管。

- 客户端只看到 CPA 的一个 base-url 和一个 key
- 账号轮询、401 冷却、失败重试、按账号统计全部复用 CPA 原生能力
- 插件补的是 CPA 没有的那部分：session 鉴权、密码续期、短信登录、积分额度

## 能力

| capability | 提供什么 |
|---|---|
| `auth_provider` | `auth.parse` 认凭证；`auth.refresh` 有密码就续期，没密码就验活 |
| `executor` | 带 `Bearer <session>` 转发到 `loomyad.xunfei.cn`，兼容流式/非流式 |
| `model_registrar` | 固定 8 个模型清单 |
| `management_api` | 短信登录、密码加号、账号列表、积分查询 |
| `quota_provider` | 每日额度条（每日 5000，用不完不结转）+ 永久积分 |

## 编译

```bash
cd plugin
CGO_ENABLED=1 go build -buildmode=c-shared -o loomy.so .
```

需要 Go 1.26+ 与 gcc。产物必须与 CPA 的 GOOS/GOARCH 一致。

## 安装

```yaml
config-version: 8
plugins:
    enabled: true
    dir: "/path/to/plugins"
    configs:
        loomy:
            enabled: true
            base-url: "https://loomyad.xunfei.cn/api/v1"   # 可选
            proxy-url: ""                                   # 可选
oauth:
    auth-dir: "/path/to/auths"
```

重启后日志应出现 `plugin registered plugin_id=loomy plugin_name=loomy-accounts version=0.2.0`。

## 加账号

**方式一：管理 API（推荐）**

```bash
# 1) 发验证码
curl -X POST http://127.0.0.1:8317/v0/management/loomy/send-code \
  -H "Authorization: Bearer <management-key>" -H "Content-Type: application/json" \
  -d '{"phone":"13800000000"}'
# → {"msgid":"..."}

# 2) 提交验证码，插件直接写 auth 文件
curl -X POST http://127.0.0.1:8317/v0/management/loomy/login \
  -H "Authorization: Bearer <management-key>" -H "Content-Type: application/json" \
  -d '{"phone":"13800000000","code":"123456","msgid":"..."}'
# → {"saved":"loomy-0000.json", ...}

# 3) 用密码加号（之后可无人值守续期）
curl -X POST http://127.0.0.1:8317/v0/management/loomy/add \
  -H "Authorization: Bearer <management-key>" -H "Content-Type: application/json" \
  -d '{"phone":"13800000000","password":"..."}'

# 4) 看账号与积分
curl -H "Authorization: Bearer <management-key>" http://127.0.0.1:8317/v0/management/loomy/accounts
curl -H "Authorization: Bearer <management-key>" "http://127.0.0.1:8317/v0/management/loomy/points?auth_index=<index>"
```

**方式二：手工放文件**

```json
{"type":"loomy","session":"<32位 session>","phone":"13800000000","password":"可选"}
```

有 `password` 的账号在 `auth.refresh` 时会自动重新登录（`getPuKey` → RSA/PKCS#1 v1.5 → `byPwd`），14 天到期不再需要人工。

**怎么给短信注册的号设密码**：`https://passport.xfyun.cn/forget`（讯飞开放平台的"忘记密码"页，短信验证后直接设新密码，规则为数字+字母 6~20 位）。设完把密码写进 auth 文件：

```json
{"type":"loomy","phone":"13800000000","password":"...","session":"..."}
```

实测（2026-11-07）：`auth.refresh` 返回 `renewed by password`，session 被换新，`NextRefreshAfter` 推到 +10 天。

## 两个踩过的坑

1. **网关失效回 200 不是 401**。登录过期时返回 `{"code":"100002","desc":"登录已失效"}` 且 HTTP 200，只看状态码会把错误当成功。插件会探测 `code` 字段转成 401。
2. **请求体里的裸中文会被拒**。同一句话，raw UTF-8 发送返回 `400 请求参数错误`，转成 `\uXXXX` 转义后正常。插件在转发前统一做非 ASCII 转义。

## 模型

所有模型统一带 `lm/` 前缀，避免与其他 provider 同名冲突（尤其是 `qwen3.8-flash` 这类被别处占用的 id）。插件转发前会剥掉前缀。

| 模型 | 上下文 | 能力 | 倍率 |
|---|---|---|---|
| `lm/spark-x2.6` | 1M | 工具调用 | x0.0（限时免费） |
| `lm/GLM-5.3-Flash` | 1M | 工具 + 看图 | x0.8 |
| `lm/DeepSeek-V4.1-Flash` | 1M | 工具 + 看图 | x2.0 |
| `lm/deepseek-v4-flash-0731` | 1M | 工具 | 未标注 |
| `lm/mimo-v2.5` | 1M | 工具 + 看图 | 未标注（上游偶发 429） |
| `lm/qwen3.8-flash` | 1M | 工具 + 看图 | 未标注 |

网关另有 3 个生图模型（`Hy-Image-3.5-preview`、`doubao-seedream-5-lite`、`qwen-image-3.0-pro`），走 `/v1/images/generations`，当前执行器未接。

## 账号池面板

CPA 面板菜单里的 **Loomy 账号池**，或直接访问：

```
http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts
```

| 区块 | 内容 |
|---|---|
| 汇总卡 | 账号数、可用积分合计、今日额度合计、异常账号数 |
| 账号表 | 手机号、账号状态、永久积分、今日剩余、可用合计、**会话剩余**（登录态天数，≤7 天标黄）、调用成功/失败、凭证文件；表下附逐行列说明 |
| 最近请求 | 插件进程内环形缓冲，最近 30 条：时间、账号、模型、状态码、耗时、积分（重启清零） |
| 模型清单 | 网关 `/models` 实时拉取：ID、名称、上下文、tools/vision、消费倍率、免费标记 |
| 刷新 | 手动刷新 + 60 秒自动轮询；查余额最多 5 并发 |

**有效期**取 auth 文件 mtime + 14 天（无显式 `expires_at` 时的估算，登录后写文件即校准）。

**访问保护**：CPA 的资源路由本身不鉴权。需要门槛就在 `plugins.configs.loomy` 里加：

```yaml
plugins:
    configs:
        loomy:
            enabled: true
            panel-token: "随机长串"
```

之后必须带 `?t=随机长串` 才能打开，否则 403。默认不设 token（保持从菜单直接点开）。

## 验证记录（CPA 8.0.53 / linux-arm64 / 2026-11-06）

### 生产实例（:8317，systemd `cliproxy`）

| 项 | 结果 |
|---|---|
| 插件安装位置 | `/home/ubuntu/cliproxy/plugins/linux/arm64/loomy-v0.2.0.so` |
| 配置 | `plugins.configs.loomy.enabled: true`（备份 `config.yaml.bak-20261106164024`） |
| 账号 | `/home/ubuntu/.cli-proxy-api/loomy-8109.json`（权限 600） |
| 模型总数 | 63，其中 7 个 `owned_by=loomy` |
| 非流式对话 | `http=200`，`content":"插件已修复"` |
| 流式对话 | `data: {"object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant"}}]}` |
| 服务状态 | `active`，`/healthz=200`，无 panic/fatal |

### 测试实例（:8399，已停止）

| 项 | 结果 |
|---|---|
| 插件注册 | `version=0.2.0` |
| 中文对话 | `http=200`，`content":"已准备就绪"` |
| 账号列表 | `host.auth.list` 返回 `loomy-8109.json` |
| 积分接口 | `{"availableBalance":35000,"balance":30000,"dailyBalance":5000}` |
| quota.fetch | `remainingFraction:1`，summary 三项 |
| 无效 session | 上游 `100002` → 401 `invalid_api_key` |

## 已知限制

1. **stream 是缓冲的**。CPA 插件 RPC 是请求-响应模型，SSE 整段读完再切块回放，牺牲首字延迟。
2. **密码登录未实测**。代码路径按 `getPuKey` + `byPwd` 实现，需要在讯飞账号中心设过密码的账号验证。
3. **session 泄露等于账号**。auth 文件含明文 session，权限务必 0600。
