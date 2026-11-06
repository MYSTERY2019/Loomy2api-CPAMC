# Loomy × CLIProxyAPI 插件 —— 交接与优化需求

> 交接对象：接手审计与优化的 AI / 工程师
> 交接时间：2026-11-07
> 分支状态：生产已上线（CPA 8.0.53，linux/arm64），插件版本 0.4.0
> 本文档的目标：让你能在 30 分钟内接上环境、看懂设计、并知道**哪里的 UI 和功能最欠打磨**

---

## 0. 一句话背景

Loomy（科大讯飞桌面 AI 助手）的模型网关是 OpenAI 兼容的，但鉴权是"登录 session 当 Bearer"、14 天过期、无 refresh token。为了让多个 Loomy 账号能被 CLIProxyAPI（下称 CPA）统一调度，写了一个 **CPA 动态库插件**（Go + cgo，`-buildmode=c-shared`），实现 auth / executor / quota / management 四类能力，并附带一个账号池面板。

当前池子：**4 个账号**，每天各 5000 每日额度 + 累计 75000 永久积分。

---

## 1. 环境在哪、怎么连

### 1.1 VPS

| 项 | 值 |
|---|---|
| 主机 | `ubuntu@132.226.69.12`（Oracle ARM，aarch64，Ubuntu 24.04） |
| SSH Key | `C:\Users\无与伦比的美丽\.ssh\oracle_arm_key`（在**用户本机**，不在 VPS 上） |
| 登录 | `ssh -i "C:\Users\无与伦比的美丽\.ssh\oracle_arm_key" -o StrictHostKeyChecking=no ubuntu@132.226.69.12` |
| 免密程度 | `sudo -n` 可用（NOPASSWD），无需密码 |

### 1.2 CPA（生产的那个）

| 项 | 值 |
|---|---|
| 服务 | systemd 单元 `cliproxy`，`ExecStart=/home/ubuntu/cliproxy/cli-proxy-api -config /home/ubuntu/cliproxy/config.yaml` |
| 工作目录 | `/home/ubuntu/cliproxy` |
| 监听 | `127.0.0.1:8317`，另绑定 tailscale `100.120.59.121:8317` |
| 版本 | 8.0.53（commit 01b24f1f） |
| 用户本机访问 | 本机已有 SSH 隧道 `-L 127.0.0.1:8317:127.0.0.1:8317`，所以**本机直接 `http://127.0.0.1:8317`** 就是那台 VPS 上的 CPA |
| 管理密钥 | `/home/ubuntu/cliproxy/config.yaml` → `management.secret-key`（自己去读，不要写进任何提交物） |

### 1.3 关键路径

```
/home/ubuntu/cliproxy/
├── cli-proxy-api                                  # 二进制
├── config.yaml                                    # 主配置（含 access.api-keys / management.secret-key）
├── config.yaml.bak-20261106164024                 # 上线前的备份（回滚用）
└── plugins/linux/arm64/
    └── loomy-v0.2.0.so                            # ← 本插件（9.4 MB）

/home/ubuntu/loomy-plugin/                         # 插件源码与构建产物
├── main.go                                        # 入口 + ABI + auth/executor/quota/management
├── panel.go                                       # 面板后端 + 内嵌 HTML/CSS/JS
├── go.mod
└── loomy.so                                       # 构建输出（install 到上面那个路径）

/home/ubuntu/.cli-proxy-api/                       # CPA 的 auth 目录
├── loomy-8109.json    # 131****8109  永久 45000
├── loomy-9128.json    # 183****9128  永久 10000
├── loomy-0537.json    # 132****0537  永久 10000
└── loomy-7357.json    # 158****7357  永久 10000
```

auth 文件格式（**含明文密码与 session，权限必须 600**）：

```json
{"type":"loomy","phone":"13800000000","password":"<密码>","session":"<32位session>"}
```

> 密码存在文件里是因为 `auth.refresh` 要用它做无人值守续期。审计时不要把它打印到日志或提交到任何仓库。

---

## 2. 架构

```
客户端（OpenAI 兼容）
      │  base_url = http://127.0.0.1:8317/v1     key = CPA 的 access.api-keys
      ▼
┌──────────────────────────────────────────────┐
│ CLIProxyAPI（8.0.53）                        │
│  · 路由：model 前缀 lm/ → provider loomy      │
│  · 账号池：4 个 auth 文件，自带轮询/冷却/重试  │
│  · 每 15 分钟跑一次 auth 刷新调度             │
└───────────────┬──────────────────────────────┘
                │ 插件 RPC（C ABI，JSON 信封）
                ▼
┌──────────────────────────────────────────────┐
│ loomy.so                                     │
│  auth.parse / auth.refresh（含密码续期）      │
│  executor（chat + images，UTF-8 转义）        │
│  quota.fetch / management / resource 面板     │
└───────────────┬──────────────────────────────┘
                │ HTTPS
                ▼
   loomyad.xunfei.cn/api/v1   （模型网关，OpenAI 兼容）
   account.xfinfr.com         （登录/续期，HMAC-SHA1 签名）
   loomy.xunfei.cn/api/invitation/bind （绑邀请码，需网页 cookie）
```

---

## 3. 构建与部署（照抄即可）

```bash
# 本机 → VPS 传源码
scp -i "<key>" plugin/main.go plugin/panel.go ubuntu@132.226.69.12:~/loomy-plugin/

# VPS 上编译（Go 1.27.3 已装在 /usr/local/go，gcc/flex 已装）
ssh -i "<key>" ubuntu@132.226.69.12 \
  "cd ~/loomy-plugin && CGO_ENABLED=1 /usr/local/go/bin/go build -buildmode=c-shared -o loomy.so ."

# 安装 + 重启（会中断约 3 秒）
ssh -i "<key>" ubuntu@132.226.69.12 \
  "sudo install -m 0755 ~/loomy-plugin/loomy.so /home/ubuntu/cliproxy/plugins/linux/arm64/loomy-v0.2.0.so && sudo systemctl restart cliproxy"

# 验活
curl -s http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts/data | head -c 300
curl -s -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <access.api-keys 里的任意一把>" -H "Content-Type: application/json" \
  -d '{"model":"lm/spark-x2.6","messages":[{"role":"user","content":"ok"}]}'
```

**回滚**：`sudo cp /home/ubuntu/cliproxy/config.yaml.bak-20261106164024 /home/ubuntu/cliproxy/config.yaml && sudo systemctl restart cliproxy`，并删掉 `loomy-v0.2.0.so`。

### 插件注册点（重要）

- `plugin.register` 返回的能力：`auth_provider / model_registrar / executor（chat-completions + images）/ management_api / quota_provider`
- 插件 id 观测值为 **`loomy`**，资源根为 `/v0/resource/plugins/loomy/`
- 注册的资源路由：`/accounts`（HTML）与 `/accounts/data`（JSON）；**CPA 的资源路由不走管理鉴权**（见 §6 安全）
- 管理路由在 `/v0/management/loomy/*`，需要 management key

---

## 4. 上游接口速查（逆向确认过的）

| 用途 | 方法 + 路径 | 鉴权 |
|---|---|---|
| 模型清单 | `GET https://loomyad.xunfei.cn/api/v1/models` | `Authorization: Bearer <session>` + `token: <session>` + `traceparent` + `loomy-version` |
| 对话 | `POST .../api/v1/chat/completions` | 同上 |
| 生图 | `POST .../api/v1/images/generations` | 同上 |
| 积分 | `GET .../api/v1/points/records` / `.../points/balance` | 同上（**只认 `token` 头时也要带上**） |
| 短信登录 | `account.xfinfr.com/login/phone/sendMsgCode` → `/checkCode` | HMAC-SHA1 签名（accessKey 硬编码在客户端） |
| 密码登录 | `account.xfinfr.com/login/account/getPuKey` → `/byPwd` | 同上；密码用 1024-bit RSA/PKCS#1 v1.5 加密 |
| 绑邀请码 | `POST loomy.xunfei.cn/api/invitation/bind` | **网页 cookie**（桌面 session 不行），需 `Origin` 头 |

积分返回结构：`{balance, dailyBalance, availableBalance, list:[...]}`；账单条目含 `consumeSource`（`daily`/`permanent`）、`dailyCycleDate`。

---

## 5. 当前已实现（功能基线）

### 插件侧

- `auth.parse`：认 `{session,phone,password}` 或裸 session 字符串
- `auth.refresh`：有密码 → `byPwd` 换新 session（实测 `renewed by password`，NextRefreshAfter +10 天）；无密码 → 只做验活
- `executor`：转发 chat（流式/非流式），`lm/` 前缀剥离，非 ASCII 统一转 `\uXXXX`
- `quota.fetch`：每日剩余比例 + 永久/今日/可用三个 metric
- `management`：`send-code / login / add / accounts / points` 五个路由
- 面板资源：`/accounts` + `/accounts/data`

### 面板（当前长这样）

```
汇总卡：账号数 / 可用积分合计 / 今日剩余合计 / 异常账号
账号表：手机号 | 账号状态 | 永久积分 | 今日剩余 | 可用合计 | 会话剩余 | 续期 | 调用成功/失败 | 凭证文件
        + 表下逐行列说明（dashed 框）
最近请求：时间 | 账号 | 模型 | 状态 | 耗时 | 积分（进程内环形缓冲，30 条，重启清零）
网关模型清单：模型 ID | 名称 | 上下文 | 能力(tools/vision) | 倍率
按钮：刷新全部 / 开启自动刷新(60s)
```

---

## 6. 已知坑与技术债（**改之前先读**）

| # | 坑 | 现状与影响 |
|---|---|---|
| 1 | 网关业务错误走 **HTTP 200 + `{"code":"100002"}`** | 插件已探测 `code` 字段转 401，否则 CPA 会把失效当成功 |
| 2 | 请求体里的**裸中文会被网关拒**（400 请求参数错误），转 `\uXXXX` 才过 | `escapeNonASCII()` 已处理；任何新增的出站请求都要走它 |
| 3 | CPA 的**资源路由不鉴权** | 面板内容（打码手机号、余额）同 tailnet 可见；插件支持可选 `panel-token`，但默认关着（关了菜单能直接点开） |
| 4 | management key 连续试错会触发 **IP 封禁 30 分钟**（`auth-files` 接口会返回 403） | 调试时别用猜的 key；正确 key 在 config.yaml |
| 5 | 资源路径根 `/` **注册不上**，必须用具体路径（`/accounts`）；子路径（`/accounts/data`）要**单独注册** | 面板取数靠这条 |
| 6 | 表头/落库的「剩余」列曾指代"会话天数"，被用户误读为积分 | 已改名「会话剩余」；新增列请写清单位 |
| 7 | 每日 5000 语义：**先扣每日，用不完不结转**，重置时刻**未确认**（实测 01:45 未回满，`dailyCycleDate` 已翻日） | 面板文案目前写"次日重新发放（时点未确认）" |
| 8 | 会话 14 天硬过期、**无 refresh token**；`userinfo/query.baseInfo` 不返回到期时间 | 面板的"会话剩余"是 `文件 mtime + 14 天` 估算，不是服务端值 |
| 9 | 图片请求**不走插件执行器**：CPA 的 `/v1/images/generations` 只认内置或 `openai-compatibility` 条目 | 所以插件里的 image 模型条目已撤，生图功能**当前不可用** |
| 10 | 插件是 c-shared 动态库，**panic 会带走整个 CPA 进程** | 新增代码必须避免 panic；网络调用都要有超时 |

---

## 7. UI 现状问题（用户原话："非常不完善"）

按严重程度列，这些是**这次交接要解决的核心**：

1. **无账号操作**：不能启用/停用、不能删号、不能手动续期、不能改备注。当前只能看。
2. **无排序/筛选/分页**：4 个号还好，20 个号就没法用了。表头不可点。
3. **最近请求不持久**：进程重启即清零；没有模型/状态筛选；没有分页。
4. **没有单号详情**：看不到某个号的调用分布、失败原因、最近错误原文。
5. **无历史趋势**：看不到每日消耗曲线、余额变化。（CPA 侧有 10 分钟粒度的 success/failed 桶，未可视化）
6. **无导出**：不能导出 CSV / 复制账号清单。
7. **空态与错误态粗糙**：`暂无请求`、`取不到模型清单` 就一行灰字，没有原因和重试。
8. **无加载骨架**：刷新时整张表闪一下。
9. **移动端未适配**：9 列表格在窄屏直接横向溢出。
10. **无主题**：固定深色，没有浅色/跟随系统。
11. **无 i18n**：全中文，WorkBuddy 有简/繁切换。
12. **交互反馈弱**：按钮无 loading/禁用态，刷新期间可重复点击。
13. **无访问控制**：见 §6-3。
14. **面板与 CPA 自身状态可能不一致**：面板自己查网关，CPA 另有 auth 状态与冷却，两边不同步。

---

## 8. 参考对象：WorkBuddy 聚合面板（用户指定对标）

访问：`http://127.0.0.1:8788/?tab=gateway`（**同一台 VPS**，8788 端口另起的一个面板，用户本机已有隧道）。它不在本仓库里，只能通过浏览器观察。

### 它有什么

- **顶部密码门**：`请输入面板访问密码 → 进入面板`
- **左侧/顶部 tab**：网关 / 账号 / 任务与福利 / 媒体 / 数据看板 / 智能体配置 / 设置 / 运行日志，右上角简繁切换 + 主题切换（Claude / Material / MiuiX / 浅色 / 深色 / 跟随系统 / 色彩配置）
- **网关页**
  - 每个版本一张卡（国际版/国内版）：`总积分 / 已用 · 账号数 / 总数`、`N 个 Key`
  - 网关默认出口切换
  - 调度器控制：`后台定时调度器: 运行中` + `立即巡检保活` / `暂停调度`
  - 最近请求表：条数选择（10/20/50/100）、分页（第 X / Y 页，共 N 条）、上一页/下一页
  - 网关模型清单：勾选列（全选/全不选/反选）+ `模型 ID / 名称 / 消费倍率 / 能力 / 上下文 / 最大输出 / 思考档位 / 默认采样参数` + `刷新模型清单`；勾选结果可一键写入客户端配置
  - 页脚提示：`每 5 秒自动刷新 · 数据来自本机反代`
- **账号页**：时间范围（今日/近 1 天/7/30/90/全部）× 任务类型 × 结果（成功/失败）× 账号 的多维筛选 + `刷新`

### 建议映射到 Loomy 面板

| WorkBuddy 的做法 | 移植到 Loomy 的建议 | 优先级 |
|---|---|---|
| 顶部密码门 | 面板加 `panel-token`（插件已支持，只差默认打开 + 引导） | P0 |
| 汇总卡（总积分/账号数/Key 数） | 已有，可加"今日已消耗"和"预估可跑任务数" | P1 |
| 调度器控制（巡检保活/暂停） | 加「立即续期全部」「暂停自动续期」——对应插件的 auth.refresh | P1 |
| 最近请求表（分页 + 页大小） | 现在只有 30 条无分页；建议落盘 + 分页 + 按账号/模型/状态筛选 | P0 |
| 5 秒自动刷新 | 现在是 60 秒，可做成可选档位（5s/30s/60s/关） | P1 |
| 模型清单可勾选 + 一键配置 | Loomy 不需要"写入客户端"，但可以做「只看免费模型」筛选 + 复制 `base_url` 片段 | P2 |
| 多维筛选（时间×类型×结果×账号） | 请求表加同样四维筛选 | P1 |
| 主题切换 | 加浅色/跟随系统 | P2 |
| tab 化布局 | 账号池 / 请求 / 模型 / 设置 四 tab，替代现在的长页面 | P0 |
| 运行日志 tab | 可选：把插件自身 refresh/executor 的关键事件落一个环形日志 | P2 |

---

## 9. 建议的优先级（给接手者的路线）

**P0（先做，都是"用不了"级别）**
1. 面板拆 tab（账号 / 请求 / 模型 / 设置），否则信息堆在一页看不完
2. 账号操作：启用/停用（改名 `.json` ↔ `.json.off` 即可，CPA 只认 `.json`）、手动续期（调 auth.refresh 同逻辑）、删除（要二次确认）
3. 最近请求落盘（sqlite 或 jsonl），加时间/账号/模型/状态筛选 + 分页
4. `panel-token` 默认开启，并在 CPA 菜单入口处给出带 token 的 URL

**P1（体验）**
5. 排序、每页条数、空态/错误态（含"重试"按钮）
6. 汇总卡补"今日已消耗 / 距上次刷新时间"
7. 「立即续期全部」「暂停自动续期」
8. 自动刷新档位 5s/30s/60s/关
9. 单号详情抽屉：余额、剩余天数、最近 20 条该号请求

**P2（锦上添花）**
10. 浅色/跟随系统主题；简繁切换
11. 余额与消耗的趋势图（用 CPA 的 10 分钟桶）
12. 导出 CSV
13. 移动端卡片化布局

---

## 10. 验收清单（改完必须跑）

```bash
# 1) 服务活着
systemctl is-active cliproxy

# 2) 插件注册（日志里应有 plugin registered plugin_id=loomy）
journalctl -u cliproxy -n 50 --no-pager | grep -i "plugin"

# 3) 面板页面 + 数据
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts
curl -s http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts/data | python3 -m json.tool | head -30

# 4) 中文对话（验证 §6-2 的转义没被破坏）
curl -s -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <key>" -H "Content-Type: application/json" \
  -d '{"model":"lm/spark-x2.6","messages":[{"role":"user","content":"用五个字回答：插件好了吗"}]}'

# 5) 流式
curl -s -N -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <key>" -H "Content-Type: application/json" \
  -d '{"model":"lm/spark-x2.6","messages":[{"role":"user","content":"say ok"}],"stream":true}' | head -c 200

# 6) 四个号都能取余额（逐个 session 打 /points/balance）
```

**回归红线**：`lm/` 前缀必须仍然剥掉后再转发；中文必须能过；401 必须映射成 401（不是 500）。

---

## 11. 安全与合规

1. **auth 文件含明文密码 + session**，权限 600；不要把内容贴进 issue/仓库/日志。
2. 资源路由不鉴权，**暴露面是整个 tailnet**（`100.120.59.121:8317`）。加 token 之前不要把面板截图发到公开渠道。
3. 这些账号是**用户本人的手机号**，池子的用途是分摊每日额度；不要拿去对外提供 API 服务——Loomy 积分协议禁止转售/出租/商业化，违规会封号且积分不退。
4. 上游 `account.xfinfr.com` 的 accessKey 是从桌面客户端逆向出来的（第三方项目 `loomy2api` / `10router` 也在用）。讯飞一旦轮换，登录与续期会同时失效——**这不是 if，是 when**，需要预留"降级为人工短信续期"的路径。

---

## 12. 交接清单

| 文件 | 说明 |
|---|---|
| `plugin/main.go` | 插件主体：ABI、auth、executor、quota、management 路由 |
| `plugin/panel.go` | 面板后端 + 内嵌 HTML/CSS/JS（**UI 优化主要改这里**） |
| `plugin/go.mod` | 无第三方依赖，纯标准库 + cgo |
| `loomy-linux-arm64.so` | 已编译产物（与 VPS 上的同一份） |
| `deploy-prod.sh` / `verify-prod.sh` | 部署与验活脚本（注意脚本里的路径是 `ubuntu@132.226.69.12`） |
| `tools/loomy-login.js` | 短信登录（桌面 session） |
| `tools/pwd-login.js` | 密码登录（RSA + byPwd），验证凭据是否可续期 |
| `tools/web-login.js` | 网页版登录 + 绑邀请码（注意：`bind` 需要真键盘输入才触发 React） |
| `tools/session-info.js` | 查账号基础信息（**不返回到期时间**） |
| `tools/list-models.js` | 解析网关模型清单 |

**不在仓库里、但你需要知道的**：WorkBuddy 面板在 `127.0.0.1:8788`，源码不在本机，只能浏览器观察；CPA 自身的管理 UI 在 `http://127.0.0.1:8317/management.html`。

---

## 13. 给接手者的三条建议

1. **先只读**：把面板和接口跑一遍，确认 §10 六项验收全绿，再动代码。
2. **改 UI 优先于改执行器**：`executor` 这条链路目前稳定，UI 才是短板；动 executor 前先把 §6 的坑过一遍。
3. **每次部署都留回滚点**：`cp config.yaml config.yaml.bak-$(date +%s)`，`systemctl restart` 后必须跑一遍 §10。
