package status

import (
	"context"
	"strings"
	"sync"
)

type quotaWrite struct {
	RowKey   string           `json:"row_key"`
	Provider string           `json:"provider"`
	Account  map[string]any   `json:"account"`
	Windows  []map[string]any `json:"windows"`
}

func quotaWindowID(w window) string {
	switch w.DurationSeconds {
	case 18000:
		return "five-hour"
	case 604800:
		return "weekly"
	default:
		if w.DurationSeconds >= 2419200 {
			return "monthly"
		}
		return ""
	}
}

// 保存真实查询时间与字段，让 CPAMP 统一接续周期；不反推实际起点、不重放旧缓存。
func (e *engine) syncQuotaObservations(parent context.Context, files []rawAccount, rows []account) {
	if !e.syncQuota {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	slots := make(chan struct{}, cap(e.client.slots))
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-slots }()
			entries := []quotaWrite{}
			indices := map[string]int{}
			for i := start; i < end; i++ {
				q := rows[i].Quota
				if q.Data == nil || q.Stale || q.Error != "" || q.Data.Scope != "codex_main" || q.ObservedAtMS <= 0 {
					continue
				}
				entry := quotaWrite{RowKey: rows[i].ID, Provider: "codex", Account: credentialTarget(files[i])}
				seen := map[string]bool{}
				valid := true
				for _, w := range q.Data.Windows {
					id := quotaWindowID(w)
					if id == "" || seen[id] || w.ResetAtMS <= e.now().UnixMilli() || !nonnegative(w.UsedPercent) || w.UsedPercent > 100 {
						valid = false
						break
					}
					seen[id] = true
					entry.Windows = append(entry.Windows, map[string]any{"provider_window_id": id, "window_kind": strings.ReplaceAll(id, "-", "_"),
						"window_mode": "fixed", "model_scope_kind": "family", "model_scope_key": "codex_main", "source": "api_query",
						"observed_at_ms": q.ObservedAtMS, "boundary_accuracy": "exact", "cycle_end_ms": w.ResetAtMS,
						"duration_seconds": w.DurationSeconds, "used_percent": w.UsedPercent, "remaining_percent": w.RemainingPercent, "plan_type": q.Data.Plan})
				}
				if valid && len(entry.Windows) > 0 {
					entries = append(entries, entry)
					indices[entry.RowKey] = i
				}
			}
			if len(entries) == 0 {
				return
			}
			var reply struct {
				Items []struct {
					RowKey   string `json:"row_key"`
					Provider string `json:"provider"`
					Inserted int    `json:"inserted_count"`
				} `json:"items"`
			}
			// 写入结果未知时只停止本轮估值；下一轮重新查询真实额度，不自动重发本次写入。
			if e.client.saveQuota(ctx, entries, &reply) != "" || len(reply.Items) != len(entries) {
				return
			}
			seen := map[string]bool{}
			for _, item := range reply.Items {
				if _, ok := indices[item.RowKey]; !ok || seen[item.RowKey] || item.Provider != "codex" || item.Inserted < 0 {
					return
				}
				seen[item.RowKey] = true
			}
			for key, i := range indices {
				rows[i].quotaSynced = seen[key]
			}
		}(start, end)
	}
	wg.Wait()
}
