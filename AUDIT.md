# v1 → v2 审计记录（2026-11-07）

审计对象：`plugin/main.go`、`plugin/panel.go`（v1，元数据 1.0.0 / 安装名 v0.2.0 / 交接文档 0.4.0）、`deploy-prod.sh`、`verify-prod.sh`。
对照：CLIProxyAPI `v8.0.53`（01b24f1）插件宿主源码、Cli-Proxy-API-Management-Center 插件页实现。
验证方式：本地编译 CPA v8.0.53（linux/arm64）+ v2 插件 + 模拟上游，跑通非流式、真流式、坏号自动切换、续期、启停、备注、删除、加号、请求日志、CSV、面板截图（桌面/手机）。**未接触真实 Loomy 上游**。

## 高危

| # | 问题 | 影响 | v2 处理 |
|---|---|---|---|
| 1 | `parseConfig` 把宿主传来的 JSON 信封 `{"config_yaml": base64…}` 当 YAML 逐行解析 | `base-url` / `proxy-url` / `panel-token` **从未生效**；交接文档 P0 “默认开 panel-token” 照做也不会起作用 | 先解 `config_yaml` 再解析，已加单测 |
| 2 | 资源路由 `/v0/resource/plugins/loomy/accounts/data` 不鉴权，返回手机号（掩码）、余额、请求、错误原文 | tailnet 内任何设备可读；叠加 #1，token 防护形同虚设 | 资源路由只返回静态页面壳；所有数据走 `/v0/management/loomy/*`，必须带 CPA 管理密钥 |
| 3 | c-shared 导出函数无 `recover` | 任一 panic（例如 base-url 写错导致 `http.NewRequest` 返回 nil 后解引用）直接带崩 CPA 进程 | 导出入口统一 recover → 错误信封；后台 goroutine 用 `goSafe` |
| 4 | `/loomy/add`、`/loomy/login` 响应（含失败响应）回传明文 session | 进浏览器历史/日志即泄露凭证 | 响应只返回文件名和掩码手机号 |
| 5 | 面板把上游错误、文件名直接拼进 `innerHTML` | 上游/文件名可注入脚本（XSS），而页面同源可调用管理接口 | 全部转义；加 CSP、`nosniff`、`no-referrer` |
| 6 | `quota.fetch`：`dailyBalance == 0` 时被当成 5000 | CPA 额度视图在额度耗尽时显示“满” | 修正，0 就是 0 |

## 中危

| # | 问题 | v2 处理 |
|---|---|---|
| 7 | 伪流式：整段读完再切 SSE，首字时间 = 全部生成时间；300 秒整体超时会截断长输出 | 真流式：`host.stream.emit` 逐事件转发；先同步读首个事件以便业务错误仍能映射成 401/429；单请求上限 15 分钟；`stream-mode: buffered` 可回退旧行为 |
| 8 | 每次调用 new 一个 `http.Transport` | 连接无法复用、空闲连接泄漏；改为按超时共享 client |
| 9 | 有密码的号每次 refresh 都重登；失败后固定 30 分钟重试 | 先探活，剩余 < 4 天或探活失败才重登；失败退避 30m→2h→6h→12h；新增“暂停自动续期”开关 |
| 10 | `/loomy/points` 不带 `auth_index` 时取第一个**任意 provider** 的文件并 `os.ReadFile`；`parseAuthJSON` 接受任何含 `session` 字段的 JSON | 改用 `host.auth.get`，并校验 `type=loomy` |
| 11 | `/tmp/loomy-debug` 存在时把完整 prompt 以 0644 写入 `/tmp/loomy-payload.log` | 删除 |
| 12 | refresh 用结构体重写凭证，丢弃未知字段，并把 `has_password`/`refresh` 写进文件 | 合并写回，保留 note/priority/disabled 等一切字段 |
| 13 | 所有业务错误码都映射 502 | 会话类 → 401（CPA 刷新/冷却），描述含“积分/余额/额度/频繁/limit” → 429 + retry-after（**关键词为推断，需真实错误样本确认**） |
| 14 | 请求记录仅内存 30 条，“积分”列恒为 0 | JSONL 落盘（默认保留 30 天）、首字时间、tokens、筛选/分页/CSV |

## 低危 / 工程

- 版本号四处不一致（1.0.0 / v0.2.0 / 0.4.0 / README v1.0.0）→ 统一 2.0.0，安装名 `loomy-v2.0.0.so`。
- 元数据 `GitHubRepository` 指向别人的仓库 → 改为本仓库。
- 声明了 images 能力但图片生成不可用 → 先从 capabilities 去掉，避免客户端误用。
- `deploy-prod.sh` 从 `/tmp/cpa-test` 拷凭证、不清理旧 .so、回滚不恢复旧 .so → 重写：校验 arm64、旧插件与配置备份到带时间戳目录、校验注册版本、失败完整回滚。
- 交接文档中 `ssh -o StrictHostKeyChecking=no` → 建议改为固定 known_hosts。

## 仍存在的风险（未修，需知情）

- 上游 `accessKeyID/accessSecret` 来自逆向且硬编码；厂商轮换后所有登录会失败。仓库须保持私有。
- 自动续期必须保存明文密码；只能靠文件 600、目录 700、仓库不入库来控制。
- 面板勾选“记住”时管理密钥存在浏览器 localStorage（与 CPA 管理中心同等级别）；在公用电脑上请取消勾选。
- 每日额度重置时点仍未确认；老凭证没有 `expires_at` 时“会话剩余”按文件修改时间估算（面板标“估”），首次自动续期或重登后变为精确值。
- 有密码但没有 `expires_at` 的老凭证，v2 第一次定时 refresh 会重登一次以写入到期时间。
