# loomy-accounts — Loomy provider plugin for CLIProxyAPI

**v2.0.0** · 实测：CLIProxyAPI 8.0.53 / linux-arm64

把 Loomy（讯飞桌面 AI 助手）接入 CPA：一个账号 = 一个 auth 文件，轮询、401 冷却、失败重试复用 CPA 原生调度；插件补 CPA 没有的部分——session 鉴权、密码续期、短信登录、积分额度，以及一个账号池控制台。

v1 → v2 的问题清单与修复见 [AUDIT.md](AUDIT.md)。

## 能力

| capability | 内容 |
|---|---|
| `auth_provider` | 识别 `{"type":"loomy","session",...}` 凭证；定时 refresh：先探活，到期前 4 天用密码重登，失败指数退避 |
| `executor` | `lm/<模型>` → loomyad 网关；**真流式**（逐事件转发）；中文自动转 `\uXXXX`；会话失效 → 401，额度类错误 → 429 让 CPA 换号 |
| `model_registrar` | 按账号拉取网关模型清单（10 分钟缓存），失败回落静态清单 |
| `quota_provider` | 每日额度条（5000/天）+ 永久积分 |
| `management_api` | 控制台数据与加号接口，全部需要 CPA 管理密钥 |

## 控制台

CPA 管理中心 → 插件页 → **Loomy 账号池**（或直接打开 `/v0/resource/plugins/loomy/accounts`）。首次打开输入 management key，只存本浏览器。

- **概览**：账号 / 积分 / 今日请求与成功率 / 平均耗时 / tokens，近 7 天请求，需要处理的账号，自动续期开关，客户端接入示例
- **账号**：排序、搜索、状态筛选；续期、短信重登、解除冷却、启用/停用、备注、删除（停用/备注/删除走 CPA 原生 `auth-files` 接口）；添加账号（密码或短信）；导出 CSV
- **请求日志**：时间范围 / 账号 / 模型 / 结果 / 关键词筛选，分页，导出 CSV（落盘，重启不丢）
- **模型**：网关清单、倍率、免费标记、一键复制调用名
- **运行日志**：自动续期、探活、加号、插件异常

资源路由只返回页面壳，不含任何数据。

## 编译

```bash
cd plugin
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o ../loomy-linux-arm64.so .
go test ./...
```

需要 Go 1.26+ 与 gcc，产物 GOOS/GOARCH 必须与 CPA 一致。

## 安装 / 升级

```bash
scp loomy-linux-arm64.so deploy-prod.sh verify-prod.sh ubuntu@<vps>:~/loomy-plugin/
ssh ubuntu@<vps> 'cd ~/loomy-plugin && bash deploy-prod.sh ./loomy-linux-arm64.so && bash verify-prod.sh'
```

`deploy-prod.sh` 会把旧 `loomy*.so` 和 config 备份到 `~/cliproxy/plugin-backups/<时间>/`，安装为 `loomy-v2.0.0.so`，注册失败或版本不对自动回滚。

```yaml
plugins:
    enabled: true
    dir: "/path/to/plugins"
    configs:
        loomy:
            enabled: true
            # 以下均可选
            base-url: "https://loomyad.xunfei.cn/api/v1"
            proxy-url: ""              # http(s):// 或 socks5://
            data-dir: ""               # 请求日志/运行日志/设置，默认 ~/.loomy-plugin（0700）
            stream-mode: "live"        # live=真流式；buffered=v1 行为（整段读完再发）
            log-retention-days: 30
```

## 加号

控制台“添加账号”即可。也可直接调管理 API：

```bash
H='-H "Authorization: Bearer <management-key>" -H "Content-Type: application/json"'
POST /v0/management/loomy/add        {"phone":"138...","password":"..."}   # 可无人值守续期
POST /v0/management/loomy/send-code  {"phone":"138..."}                     # → {"msgid"}
POST /v0/management/loomy/login      {"phone":"138...","code":"123456","msgid":"..."}
POST /v0/management/loomy/renew      {"auth_index":"..."}                   # 立即用密码重登
GET  /v0/management/loomy/overview | /requests | /models | /events
```

同一手机号再次登录会原地更新原文件，备注/优先级/停用状态保留；短信重登不会抹掉已存的密码。

手工放文件也可以：`{"type":"loomy","session":"<session>","phone":"138...","password":"可选"}`，权限 600。

## 已知限制

- 上游签名密钥来自逆向，厂商轮换后登录会失败；仓库保持私有。
- 自动续期需要明文密码存放在凭证文件里。
- 每日额度重置时点未确认；老凭证无 `expires_at` 时会话剩余为估算值。
- 图片生成暂不支持。
