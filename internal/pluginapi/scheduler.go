package pluginapi

import (
	"context"
	"sort"

	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

// 浏览器来源先排队再进入 Runner，避免等待 WebView 槽位耗尽来源的搜索与解析预算。
// 槽位在请求间共享；直接在线与 BT 来源不占用浏览器槽位。
func (handler *Handler) startSources(ctx context.Context, active []sourceruntime.Runner, request sourceruntime.ResolveRequest, messages chan<- candidateMessage) {
	var queued []int
	for index, runner := range active {
		if needsBrowser(runner.Source()) {
			queued = append(queued, index)
		} else {
			go runSource(ctx, index, runner, request, messages)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		a, b := active[queued[i]].Source(), active[queued[j]].Source()
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return a.ID < b.ID
	})
	go func() {
		for _, index := range queued {
			select {
			case handler.browserSlots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				<-handler.browserSlots
				return
			}
			go func(index int) {
				defer func() { <-handler.browserSlots }()
				runSource(ctx, index, active[index], request, messages)
			}(index)
		}
	}()
}

func needsBrowser(source sourceruntime.Source) bool {
	for _, capability := range source.Capabilities {
		if capability == "browser_sniff" {
			return true
		}
	}
	return false
}
