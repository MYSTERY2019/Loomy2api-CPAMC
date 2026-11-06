package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type accountRow struct {
	Phone     string `json:"phone"`
	File      string `json:"file"`
	Status    string `json:"status"`
	Permanent int64  `json:"permanent"`
	Daily     int64  `json:"daily"`
	Available int64  `json:"available"`
	DaysLeft  int    `json:"daysLeft"`
	AutoRenew bool   `json:"autoRenew"`
	Success   int64  `json:"success"`
	Failed    int64  `json:"failed"`
	Error     string `json:"error,omitempty"`
}

type panelSummary struct {
	Accounts int64 `json:"accounts"`
	Available int64 `json:"available"`
	Today     int64 `json:"today"`
	Unhealthy int64 `json:"unhealthy"`
}

type modelRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Context  int64  `json:"context"`
	Tools    bool   `json:"tools"`
	Vision   bool   `json:"vision"`
	Multiple string `json:"multiple"`
	Free     bool   `json:"free"`
}

type panelData struct {
	UpdatedAt string       `json:"updatedAt"`
	Summary   panelSummary `json:"summary"`
	Accounts  []accountRow `json:"accounts"`
	Requests  []reqLog     `json:"requests"`
	Models    []modelRow   `json:"models"`
}

type authListEntry struct {
	Path           string `json:"path"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	ModTime        string `json:"modtime"`
	RecentRequests []struct {
		Time    string `json:"time"`
		Success int64  `json:"success"`
		Failed  int64  `json:"failed"`
	} `json:"recent_requests"`
}

// buildPanelData reads every loomy auth file, queries balances and assembles the payload.
func buildPanelData() (panelData, error) {
	raw, err := callHost("host.auth.list", map[string]any{})
	if err != nil {
		return panelData{}, err
	}
	var listing struct {
		Files []authListEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return panelData{}, err
	}

	files := make([]authListEntry, 0, len(listing.Files))
	for _, f := range listing.Files {
		if strings.EqualFold(f.Provider, providerKey) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, 5)
	)
	rows := make([]accountRow, 0, len(files))
	for _, f := range files {
		wg.Add(1)
		go func(f authListEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := accountRow{File: f.Name, Status: "ok"}
			for _, b := range f.RecentRequests {
				row.Success += b.Success
				row.Failed += b.Failed
			}

			data, errRead := os.ReadFile(f.Path)
			if errRead != nil {
				row.Status, row.Error = "error", errRead.Error()
			} else if auth, errParse := parseAuthJSON(data); errParse != nil || auth.Session == "" {
				row.Status, row.Error = "error", "no session"
			} else {
				row.Phone = maskPhone(auth.Phone)
				row.AutoRenew = auth.Password != ""
				row.DaysLeft = daysLeft(f.Path, auth.ExpiresAt)
				points, errPoints := fetchPoints(auth.Session)
				if errPoints != nil {
					row.Status, row.Error = "invalid", errPoints.Error()
				} else {
					row.Permanent = int64(toFloat(points["balance"]))
					row.Daily = int64(toFloat(points["dailyBalance"]))
					row.Available = int64(toFloat(points["availableBalance"]))
				}
			}
			mu.Lock()
			rows = append(rows, row)
			mu.Unlock()
		}(f)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].File < rows[j].File })

	sum := panelSummary{Accounts: int64(len(rows))}
	for _, r := range rows {
		sum.Available += r.Available
		sum.Today += r.Daily
		if r.Status != "ok" {
			sum.Unhealthy++
		}
	}

	return panelData{
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
		Summary:   sum,
		Accounts:  rows,
		Requests:  snapshotRequests(),
		Models:    buildModelRows(rows, files),
	}, nil
}

// daysLeft estimates session lifetime: explicit expiry, else file mtime + 14 days.
func daysLeft(path, expires string) int {
	var exp time.Time
	if expires != "" {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			exp = t
		}
	}
	if exp.IsZero() {
		if st, err := os.Stat(path); err == nil {
			exp = st.ModTime().Add(14 * 24 * time.Hour)
		}
	}
	if exp.IsZero() {
		return 0
	}
	d := int(time.Until(exp).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// buildModelRows pulls the gateway catalogue once, using any live session.
func buildModelRows(rows []accountRow, files []authListEntry) []modelRow {
	for i, f := range files {
		if i >= len(rows) || rows[i].Status != "ok" {
			continue
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		auth, err := parseAuthJSON(data)
		if err != nil || auth.Session == "" {
			continue
		}
		models, err := fetchModelCatalog(auth.Session)
		if err == nil && len(models) > 0 {
			return models
		}
	}
	return nil
}

func fetchModelCatalog(session string) ([]modelRow, error) {
	req, _ := http.NewRequest(http.MethodGet, gateway(modelsPath), nil)
	setAuthHeaders(req, session)
	resp, err := httpClient(15 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
			Capabilities  struct {
				Vision          bool `json:"vision"`
				FunctionCalling bool `json:"function_calling"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]modelRow, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" || m.ID == "auto" {
			continue
		}
		row := modelRow{
			ID:      m.ID,
			Name:    m.Name,
			Context: m.ContextLength,
			Tools:   m.Capabilities.FunctionCalling,
			Vision:  m.Capabilities.Vision,
		}
		row.Multiple, row.Free = parseMultiple(m.Name)
		out = append(out, row)
	}
	return out, nil
}

// parseMultiple reads the "(x2.0)" / "限时免费" markers out of the display name.
func parseMultiple(name string) (string, bool) {
	n := strings.NewReplacer("（", "(", "）", ")", " ", "", "\u00a0", "").Replace(name)
	if strings.Contains(n, "限时免费") {
		return "0.0", true
	}
	start := strings.LastIndex(n, "(x")
	if start < 0 {
		return "—", false
	}
	end := strings.Index(n[start:], ")")
	if end < 0 {
		return "—", false
	}
	val := strings.TrimPrefix(n[start:start+end], "(x")
	if val == "0.0" {
		return val, true
	}
	return val, false
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func maskPhone(p string) string {
	if len(p) == 11 {
		return p[:3] + "****" + p[7:]
	}
	return p
}

// handlePanel serves the browser resource (list page + json data).
func handlePanel(method, path string, query map[string][]string) ([]byte, error) {
	if panelToken != "" {
		tok := ""
		if v, ok := query["t"]; ok && len(v) > 0 {
			tok = v[0]
		}
		if tok != panelToken {
			return mgmtJSON(http.StatusForbidden, map[string]any{"error": "append ?t=<panel-token> to open this page"}), nil
		}
	}
	clean := strings.TrimSuffix(path, "/")
	switch {
	case method == "GET" && strings.HasSuffix(clean, "data"):
		data, err := buildPanelData()
		if err != nil {
			return mgmtJSON(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
		}
		return mgmtJSON(http.StatusOK, data), nil
	case method == "GET":
		return mgmtHTML(panelHTML), nil
	default:
		return mgmtJSON(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
}

func mgmtHTML(page string) []byte {
	resp := map[string]any{
		"StatusCode": http.StatusOK,
		"Headers":    map[string][]string{"content-type": {"text/html; charset=utf-8"}},
		"Body":       []byte(page),
	}
	out, _ := okEnvelope(resp)
	return out
}

const panelHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Loomy 账号池</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e9ef;--dim:#8b93a7;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--accent:#4c8dff}
.legend b{color:var(--fg)}
.legend .lt{color:var(--fg);margin-bottom:4px}
.legend div{line-height:1.8}
.legend i{vertical-align:middle}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.6 -apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,"Microsoft YaHei",sans-serif}
.wrap{max-width:980px;margin:0 auto;padding:26px 20px 60px}
h1{font-size:20px;margin:0 0 4px}
h2{font-size:14px;margin:26px 0 10px;color:var(--dim);font-weight:500;letter-spacing:.3px}
.sub{color:var(--dim);font-size:13px;margin-bottom:18px}
.cards{display:grid;grid-template-columns:repeat(4,1fr);gap:10px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.card .k{color:var(--dim);font-size:12px}
.card .v{font-size:20px;font-variant-numeric:tabular-nums;margin-top:2px}
.bar{display:flex;gap:10px;align-items:center;margin:18px 0 10px;flex-wrap:wrap}
button{background:var(--accent);color:#fff;border:0;border-radius:6px;padding:8px 14px;font-size:13px;cursor:pointer}
button.ghost{background:transparent;border:1px solid var(--line);color:var(--fg)}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:10px;overflow:hidden}
th,td{padding:9px 12px;text-align:left;border-bottom:1px solid var(--line);font-size:13px}
th{color:var(--dim);font-weight:500;background:#12151b}
tr:last-child td{border-bottom:0}
.num{font-variant-numeric:tabular-nums;text-align:right}
.dot{display:inline-block;width:7px;height:7px;border-radius:50%;margin-right:6px}
.ok{background:var(--ok)}.warn{background:var(--warn)}.bad{background:var(--bad)}
.tag{color:var(--dim);font-size:12px}
.pill{display:inline-block;padding:1px 7px;border-radius:999px;font-size:11px;border:1px solid var(--line);color:var(--dim);margin-right:4px}
.pill.on{color:#7ee787;border-color:#23532f;background:#0f2417}
.empty{padding:22px;text-align:center;color:var(--dim)}
#msg{color:var(--dim);font-size:12px}
</style></head>
<body><div class="wrap">
<h1>Loomy 账号池</h1>
<div class="sub">每个账号一个 auth 文件，CPA 负责轮询与失败冷却；余额为网关实时值。</div>

<div class="cards">
  <div class="card"><div class="k">账号数</div><div class="v" id="s-accounts">–</div></div>
  <div class="card"><div class="k">可用积分合计</div><div class="v" id="s-available">–</div></div>
  <div class="card"><div class="k">今日剩余合计</div><div class="v" id="s-today">–</div></div>
  <div class="card"><div class="k">异常账号</div><div class="v" id="s-unhealthy">–</div></div>
</div>

<div class="bar">
  <button id="refresh">刷新全部</button>
  <button id="auto" class="ghost">开启自动刷新</button>
  <span id="msg">加载中…</span>
</div>

<h2>账号</h2>
<table><thead><tr>
  <th>手机号</th><th>账号状态</th>
  <th class="num" title="永久积分：不清零，用完即止">永久积分</th>
  <th class="num" title="今日剩余：每天发放 5000 的燃烧池，先扣这里，用不完不结转">今日剩余</th>
  <th class="num" title="可用合计 = 永久积分 + 今日剩余">可用合计</th>
  <th class="num" title="登录会话还能用几天（14 天到期后需重新登录，否则该号无法调用）">会话剩余</th>
  <th title="自动 = 凭证里存了密码，到期前会自己续；手动 = 到期要重新收短信">续期</th>
  <th class="num" title="CPA 侧最近约 3 小时的调用成功/失败次数">调用成功/失败</th>
  <th>凭证文件</th>
</tr></thead><tbody id="rows"><tr><td colspan="9" class="empty">加载中…</td></tr></tbody></table>
<div class="legend">
  <div class="lt">面板列说明</div>
  <div><b>账号状态</b>　正常 = 会话可用；会话失效 = 网关拒绝，需重新登录；错误 = 读不到凭证文件</div>
  <div><b>永久积分</b>　不清零，用完即止；今日额度烧完才会动它</div>
  <div><b>今日剩余</b>　每号每天发 5000，先扣这里；<b>用不完不结转</b>，次日重新发放（具体时点未确认）</div>
  <div><b>可用合计</b>　永久积分 + 今日剩余</div>
  <div><b>会话剩余</b>　登录态 14 天硬过期，<b>与积分无关</b>；到期不重登，这个号就掉线</div>
</div>

<h2>最近请求（插件进程内，最多 30 条）</h2>
<table><thead><tr>
  <th>时间</th><th>账号</th><th>模型</th><th>状态</th><th class="num">耗时</th><th class="num">积分</th>
</tr></thead><tbody id="reqs"><tr><td colspan="6" class="empty">加载中…</td></tr></tbody></table>

<h2>网关模型清单</h2>
<div class="sub" style="margin:0 0 8px">数据来自 loomyad /models，括号内为消费倍率。</div>
<table><thead><tr>
  <th>模型 ID</th><th>名称</th><th class="num">上下文</th><th>能力</th><th class="num">倍率</th>
</tr></thead><tbody id="models"><tr><td colspan="5" class="empty">加载中…</td></tr></tbody></table>
</div>
<script>
let timer=null;
const fmt=n=>(n||0).toLocaleString('en-US');
const kctx=n=>n>=1048576?(n/1048576)+'M':(n>=1000?(n/1000).toFixed(0)+'K':n);
function statusOf(r){
  if(r.status==='ok') return '<span class="dot ok"></span>正常';
  if(r.status==='invalid') return '<span class="dot warn"></span>会话失效';
  return '<span class="dot bad"></span>错误';
}
function daysCell(d){
  if(!d) return '<span class="tag">未知</span>';
  const cls=d<=7?'color:#d29922':'';
  return '<span style="'+cls+'">'+d+' 天</span>';
}
async function load(){
  const msg=document.getElementById('msg');
  msg.textContent='查询中…';
  try{
    const res=await fetch(location.pathname.replace(/\/$/,'')+'/data'+location.search,{cache:'no-store'});
    const d=await res.json();
    if(d.error){msg.textContent='错误：'+d.error;return;}
    const s=d.summary||{};
    document.getElementById('s-accounts').textContent=s.accounts||0;
    document.getElementById('s-available').textContent=fmt(s.available);
    document.getElementById('s-today').textContent=fmt(s.today);
    document.getElementById('s-unhealthy').textContent=s.unhealthy||0;

    const rows=d.accounts||[];
    document.getElementById('rows').innerHTML=rows.length?rows.map(r=>'<tr>'+
      '<td>'+r.phone+'</td>'+
      '<td>'+statusOf(r)+(r.error?'<div class="tag">'+r.error.slice(0,50)+'</div>':'')+'</td>'+
      '<td class="num">'+fmt(r.permanent)+'</td>'+
      '<td class="num">'+fmt(r.daily)+'</td>'+
      '<td class="num">'+fmt(r.available)+'</td>'+
      '<td class="num">'+daysCell(r.daysLeft)+'</td>'+
      '<td>'+(r.autoRenew?'<span class="pill on">自动</span>':'<span class="pill">手动</span>')+'</td>'+
      '<td class="num">'+fmt(r.success)+' / '+fmt(r.failed)+'</td>'+
      '<td class="tag">'+r.file+'</td></tr>').join(''):
      '<tr><td colspan="9" class="empty">还没有账号</td></tr>';

    const reqs=d.requests||[];
    document.getElementById('reqs').innerHTML=reqs.length?reqs.map(q=>'<tr>'+
      '<td class="tag">'+q.time+'</td>'+
      '<td>'+q.account+'</td>'+
      '<td>'+q.model+'</td>'+
      '<td>'+(q.status===200?'<span class="dot ok"></span>200':'<span class="dot bad"></span>'+q.status)+'</td>'+
      '<td class="num">'+q.ms+' ms</td>'+
      '<td class="num">'+fmt(q.points)+'</td></tr>').join(''):
      '<tr><td colspan="6" class="empty">暂无请求（进程重启后清零）</td></tr>';

    const ms=d.models||[];
    document.getElementById('models').innerHTML=ms.length?ms.map(m=>'<tr>'+
      '<td>'+m.id+'</td>'+
      '<td class="tag">'+(m.name||'')+'</td>'+
      '<td class="num">'+kctx(m.context)+'</td>'+
      '<td>'+(m.tools?'<span class="pill on">tools</span>':'')+(m.vision?'<span class="pill on">vision</span>':'')+'</td>'+
      '<td class="num">'+(m.free?'<span class="pill on">免费</span>':'x'+m.multiple)+'</td></tr>').join(''):
      '<tr><td colspan="5" class="empty">取不到模型清单</td></tr>';

    msg.textContent='更新于 '+d.updatedAt;
  }catch(e){msg.textContent='查询失败：'+e.message;}
}
document.getElementById('refresh').onclick=load;
document.getElementById('auto').onclick=(e)=>{
  if(timer){clearInterval(timer);timer=null;e.target.textContent='开启自动刷新';e.target.classList.add('ghost');}
  else{timer=setInterval(load,60000);e.target.textContent='停止自动刷新';e.target.classList.remove('ghost');}
};
load();
</script></body></html>`

var _ = fmt.Sprintf
