// analysis.go — 智能外呼无效沟通根因分析（可复现脚本）
//
// 运行：go run analysis.go
// 依赖：仅 Go 标准库；读取同目录下的 outbound_calls.csv
// 输出：Markdown 格式的全部统计结果，README.md 中引用的数字均由本脚本产出
//
// 成本口径（题目给定）：每次接听秒挂损失 90 元；每次标记暂缓外呼花费 10 元。
package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hangupCost    = 90.0 // 每次秒挂损失（元）
	deferCost     = 10.0 // 每次暂缓外呼成本（元）
	breakEvenRate = deferCost / hangupCost
)

// Connected: 1=有效接通沟通, 0=秒挂/拒接, -1=结果未回传
type Call struct {
	ID          string
	CallTime    time.Time
	SyncedAt    time.Time
	Team        string
	Campaign    string
	SilenceDays int
	LeadScore   int
	Connected   int
	LagH        float64 // 画像滞后时长（小时）= call_time - profile_synced_at
}

type agg struct {
	n, conn, hangup, unknown int
	sumLag, sumSilence       float64
}

func (a *agg) add(c Call) {
	a.n++
	switch c.Connected {
	case 1:
		a.conn++
	case 0:
		a.hangup++
	default:
		a.unknown++
	}
	a.sumLag += c.LagH
	a.sumSilence += float64(c.SilenceDays)
}
func (a *agg) known() int { return a.conn + a.hangup }
func (a *agg) rate() float64 { // 秒挂率（分母=已回传结果）
	if a.known() == 0 {
		return 0
	}
	return float64(a.hangup) / float64(a.known())
}
func (a *agg) rateStr() string { return pct(a.rate()) }

func pct(x float64) string     { return fmt.Sprintf("%.1f%%", x*100) }
func money(x float64) string   { return fmt.Sprintf("%.0f", x) }
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func main() {
	calls, parseErrs := load("outbound_calls.csv")

	// ============ 1. 数据体检 ============
	var all agg
	negLag, knownRows := 0, []Call{}
	lags, silences := []float64{}, []float64{}
	for _, c := range calls {
		all.add(c)
		if c.LagH < 0 {
			negLag++
		}
		if c.Connected >= 0 {
			knownRows = append(knownRows, c)
			lags = append(lags, c.LagH)
			silences = append(silences, float64(c.SilenceDays))
		}
	}
	sort.Float64s(lags)
	q := func(p float64) float64 {
		i := int(p * float64(len(lags)-1))
		return lags[i]
	}

	fmt.Println("## 1. 数据体检")
	fmt.Printf("- 总行数: %d；结果回传: %d（1=%d, 0=%d），未回传: %d（%s）\n",
		all.n, all.known(), all.conn, all.hangup, all.unknown, pct(float64(all.unknown)/float64(all.n)))
	fmt.Printf("- 有效接通率（分母=已回传）: %s；秒挂率: %s\n", pct(float64(all.conn)/float64(all.known())), all.rateStr())
	fmt.Printf("- 画像滞后 lag=call_time-profile_synced_at: 中位数 %.1fh, P90 %.1fh, 最大 %.1fh；负值(同步晚于呼叫) %d 行\n",
		median(lags), q(0.9), lags[len(lags)-1], negLag)
	fmt.Printf("- 时间戳解析失败 %d 行；silence_days/lead_score 范围见原始数据\n\n", parseErrs)

	// ============ 2. 核心验证：滞后 × 秒挂率 ============
	fmt.Println("## 2. 画像滞后时长 × 秒挂率")
	fmt.Println("| 滞后区间 | 呼叫量 | 已回传 | 秒挂(0) | 接通(1) | 秒挂率 | 中位滞后(h) |")
	fmt.Println("|---|---|---|---|---|---|---|")
	buckets := []struct {
		label   string
		lo, hi  float64
	}{
		{"<0(异常)", -1e18, 0},
		{"0-1h", 0, 1},
		{"1-6h", 1, 6},
		{"6-24h", 6, 24},
		{"1-3天", 24, 72},
		{"3-7天", 72, 168},
		{">7天", 168, 1e18},
	}
	for _, b := range buckets {
		var a agg
		med := []float64{}
		for _, c := range calls {
			if c.LagH >= b.lo && c.LagH < b.hi {
				a.add(c)
				if c.Connected >= 0 {
					med = append(med, c.LagH)
				}
			}
		}
		if a.n == 0 {
			continue
		}
		fmt.Printf("| %s | %d | %d | %d | %d | %s | %.1f |\n",
			b.label, a.n, a.known(), a.hangup, a.conn, a.rateStr(), median(med))
	}
	fmt.Println()

	// 机制验证：秒挂 vs 接通 两组的滞后与静默天数对比
	fmt.Println("## 3. 机制验证：秒挂组 vs 接通组")
	hangLags, hangSil, connLags, connSil := []float64{}, []float64{}, []float64{}, []float64{}
	for _, c := range knownRows {
		if c.Connected == 0 {
			hangLags = append(hangLags, c.LagH)
			hangSil = append(hangSil, float64(c.SilenceDays))
		} else {
			connLags = append(connLags, c.LagH)
			connSil = append(connSil, float64(c.SilenceDays))
		}
	}
	fmt.Printf("- 秒挂组: 中位滞后 %.1fh，中位静默 %.0f 天\n", median(hangLags), median(hangSil))
	fmt.Printf("- 接通组: 中位滞后 %.1fh，中位静默 %.0f 天\n\n", median(connLags), median(connSil))

	// silence_days 分桶 × 秒挂率
	fmt.Println("## 4. 静默天数 × 秒挂率")
	fmt.Println("| silence_days | 呼叫量 | 秒挂率 |")
	fmt.Println("|---|---|---|")
	sb := []struct {
		label    string
		lo, hi   float64
	}{
		{"0", 0, 1}, {"1-3", 1, 4}, {"4-7", 4, 8}, {"8-14", 8, 15}, {"15-30", 15, 31}, {">30", 31, 1e18},
	}
	for _, b := range sb {
		var a agg
		for _, c := range calls {
			if float64(c.SilenceDays) >= b.lo && float64(c.SilenceDays) < b.hi {
				a.add(c)
			}
		}
		if a.n == 0 {
			continue
		}
		fmt.Printf("| %s | %d | %s |\n", b.label, a.n, a.rateStr())
	}
	fmt.Println()

	// ============ 4b. 交叉验证：silence_days × lag ============
	sb2 := []struct {
		label    string
		slo, shi float64 // silence_days 区间
	}{
		{"0", 0, 1}, {"1-3", 1, 4}, {"4-7", 4, 8}, {"8+", 8, 1e18},
	}
	lagRanges := []struct {
		label   string
		lo, hi  float64
	}{
		{"≤0.5h", -1e18, 0.5},
		{">0.5h", 0.5, 1e18},
	}
	fmt.Println("## 4b. 交叉验证：silence_days(分组) × 滞后(≤0.5h / >0.5h)")
	fmt.Println("| silence_days | 滞后 | 呼叫量 | 已回传 | 秒挂 | 秒挂率 |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, sb := range sb2 {
		for _, lb := range lagRanges {
			var a agg
			for _, c := range calls {
				inLag := c.LagH > 0.5 // 与规则A口径一致
				if lb.hi == 0.5 {
					inLag = c.LagH <= 0.5
				}
				if float64(c.SilenceDays) >= sb.slo && float64(c.SilenceDays) < sb.shi && inLag {
					a.add(c)
				}
			}
			if a.n == 0 {
				continue
			}
			fmt.Printf("| %s | %s | %d | %d | %d | %s |\n", sb.label, lb.label, a.n, a.known(), a.hangup, a.rateStr())
		}
	}
	fmt.Println()

	// lag>0.5h 与 silence_days=0 的重合度
	both, lagOnly, silOnly, neither := 0, 0, 0, 0
	for _, c := range calls {
		lagHit, silHit := c.LagH > 0.5, c.SilenceDays == 0
		switch {
		case lagHit && silHit:
			both++
		case lagHit:
			lagOnly++
		case silHit:
			silOnly++
		default:
			neither++
		}
	}
	fmt.Printf("规则重合度: 两者都命中=%d, 仅lag>0.5h=%d, 仅silence_days=0=%d, 都不命中=%d\n\n", both, lagOnly, silOnly, neither)

	// ============ 4c. lead_score × 秒挂率 ============
	fmt.Println("## 4c. lead_score 分桶 × 秒挂率")
	fmt.Println("| lead_score | 呼叫量 | 已回传 | 秒挂 | 秒挂率 |")
	fmt.Println("|---|---|---|---|---|")
	lb := []struct {
		label  string
		lo, hi float64
	}{
		{"0-39", 0, 40}, {"40-59", 40, 60}, {"60-79", 60, 80}, {"80-100", 80, 101},
	}
	for _, b := range lb {
		var a agg
		for _, c := range calls {
			if float64(c.LeadScore) >= b.lo && float64(c.LeadScore) < b.hi {
				a.add(c)
			}
		}
		if a.n == 0 {
			continue
		}
		fmt.Printf("| %s | %d | %d | %d | %s |\n", b.label, a.n, a.known(), a.hangup, a.rateStr())
	}
	fmt.Println()

	// ============ 4d. silence_days=0 的 38 通电话长什么样 ============
	fmt.Println("## 4d. silence_days=0 的呼叫拆解")
	fmt.Println("| 维度 | 值 | 呼叫量 | 已回传 | 秒挂 | 秒挂率 |")
	fmt.Println("|---|---|---|---|---|---|")
	dimStat(calls, "team")
	dimStat(calls, "campaign")
	fmt.Println()

	// ============ 5. 维度拆解：团队 / 场景 ============
	fmt.Println("## 5. 团队与场景拆解")
	fmt.Println("| team_id | 呼叫量 | 秒挂 | 秒挂率 | 中位滞后(h) | 秒挂损失(元) |")
	fmt.Println("|---|---|---|---|---|---|")
	teamStat(calls, "team")
	fmt.Println()
	fmt.Println("| campaign_type | 呼叫量 | 秒挂 | 秒挂率 | 中位滞后(h) | 秒挂损失(元) |")
	fmt.Println("|---|---|---|---|---|---|")
	teamStat(calls, "campaign")
	fmt.Println()

	// ============ 6. 算钱：暂缓规则模拟 ============
	fmt.Println("## 6. 规则模拟：滞后超过阈值 T 则暂缓外呼")
	fmt.Printf("- 盈亏平衡点：命中率秒挂率 > %.1f%%（=10/90）时暂缓才有正收益\n", breakEvenRate*100)
	fmt.Printf("- 当前总浪费：秒挂 %d 次 × 90 元 = %s 元\n\n", all.hangup, money(float64(all.hangup)*hangupCost))
	fmt.Println("| 阈值 T | 暂缓量 | 组内秒挂率 | 避免秒挂(次) | 暂缓成本(元) | 净节省(元) |")
	fmt.Println("|---|---|---|---|---|---|")
	bestSave, bestT := 0.0, 0.0
	for _, t := range []float64{0.5, 1, 2, 4, 8, 12, 24, 36, 48, 72, 168} {
		n, hang := 0, 0
		for _, c := range knownRows {
			if c.LagH > t {
				n++
				if c.Connected == 0 {
					hang++
				}
			}
		}
		if n == 0 {
			continue
		}
		r := float64(hang) / float64(n)
		save := float64(hang)*hangupCost - float64(n)*deferCost
		fmt.Printf("| >%gh | %d | %s | %.1f | %s | %s |\n", t, n, pct(r), float64(hang), money(float64(n)*deferCost), money(save))
		if save > bestSave {
			bestSave, bestT = save, t
		}
	}
	fmt.Printf("\n**最优阈值: lag > %g 小时暂缓, 净节省 %s 元, 占当前总浪费的 %s**\n\n",
		bestT, money(bestSave), pct(bestSave/(float64(all.hangup)*hangupCost)))

	// 规则组合对比（基于已回传结果的呼叫）
	fmt.Println("### 规则组合对比（暂缓命中呼叫，按已回传结果估算）")
	runRule := func(name string, hit func(Call) bool) {
		n, hang := 0, 0
		for _, c := range knownRows {
			if hit(c) {
				n++
				if c.Connected == 0 {
					hang++
				}
			}
		}
		save := float64(hang)*hangupCost - float64(n)*deferCost
		fmt.Printf("- %s: 命中 %d 通, 组内秒挂率 %s, 避免秒挂 %d 次, 净节省 %s 元\n",
			name, n, pct(float64(hang)/float64(n)), hang, money(save))
	}
	runRule("规则A: lag>0.5h 暂缓", func(c Call) bool { return c.LagH > 0.5 })
	runRule("规则B: silence_days=0 暂缓", func(c Call) bool { return c.SilenceDays == 0 })
	runRule("规则A∨B: lag>0.5h 或 silence_days=0", func(c Call) bool { return c.LagH > 0.5 || c.SilenceDays == 0 })
	fmt.Printf("- 参考上限: 全量暂缓(所有秒挂都避免) 净节省 %s 元\n\n", money(float64(all.hangup)*hangupCost-float64(all.known())*deferCost))

	// 团队 × 滞后：哪个团队的同步纪律最差
	fmt.Println("## 7. 各团队画像滞后中位数（同步纪律）")
	teamStatLag(calls)
}

func rateStr(a *agg) string { return pct(a.rate()) }

// dimStat: silence_days=0 的呼叫按维度拆解
func dimStat(calls []Call, dim string) {
	m := map[string]*agg{}
	for _, c := range calls {
		if c.SilenceDays != 0 {
			continue
		}
		k := c.Team
		if dim == "campaign" {
			k = c.Campaign
		}
		if m[k] == nil {
			m[k] = &agg{}
		}
		m[k].add(c)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dimName := "team_id"
	if dim == "campaign" {
		dimName = "campaign_type"
	}
	for _, k := range keys {
		a := m[k]
		fmt.Printf("| %s | %s | %d | %d | %d | %s |\n", dimName, k, a.n, a.known(), a.hangup, a.rateStr())
	}
}

func teamStat(calls []Call, dim string) {
	m := map[string]*agg{}
	med := map[string][]float64{}
	for i := range calls {
		c := &calls[i]
		k := c.Team
		if dim == "campaign" {
			k = c.Campaign
		}
		if m[k] == nil {
			m[k] = &agg{}
		}
		m[k].add(*c)
		if c.Connected == 0 {
			med[k] = append(med[k], c.LagH)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := m[k]
		fmt.Printf("| %s | %d | %d | %s | %.1f | %s |\n", k, a.n, a.hangup, a.rateStr(), median(med[k]), money(float64(a.hangup)*hangupCost))
	}
}

func teamStatLag(calls []Call) {
	m := map[string][]float64{}
	for i := range calls {
		m[calls[i].Team] = append(m[calls[i].Team], calls[i].LagH)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("- %s: 中位 %.1fh\n", k, median(m[k]))
	}
}

func load(path string) ([]Call, int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("打开 CSV 失败:", err)
		os.Exit(1)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		fmt.Println("解析 CSV 失败:", err)
		os.Exit(1)
	}
	idx := map[string]int{}
	for i, h := range records[0] {
		idx[strings.TrimSpace(h)] = i
	}
	calls, errs := []Call{}, 0
	for _, row := range records[1:] {
		get := func(col string) string { return strings.TrimSpace(row[idx[col]]) }
		ct, err1 := time.Parse(time.RFC3339, get("call_time"))
		pt, err2 := time.Parse(time.RFC3339, get("profile_synced_at"))
		sd, err3 := strconv.Atoi(get("silence_days"))
		ls, err4 := strconv.Atoi(get("lead_score"))
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			errs++
			continue
		}
		conn := -1
		if v := get("is_connected"); v != "" {
			if conn, err = strconv.Atoi(v); err != nil {
				conn = -1
			}
		}
		calls = append(calls, Call{
			ID: get("call_id"), CallTime: ct, SyncedAt: pt,
			Team: get("team_id"), Campaign: get("campaign_type"),
			SilenceDays: sd, LeadScore: ls, Connected: conn,
			LagH: ct.Sub(pt).Hours(),
		})
	}
	return calls, errs
}
