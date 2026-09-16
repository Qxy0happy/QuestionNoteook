// Package devtz 记住**设备**的时区，供各服务算「今天」用。
//
// 为什么需要这么一个包：**Go 在安卓上拿不到系统时区** —— 没有 /etc/localtime、没有 $TZ、
// 也没有 GOROOT 里的 zoneinfo.zip，于是 time.Local 就是 UTC。
//
// 后果不是「显示差八小时」那么轻，「今天」的边界会整个错：
//
//   - 复习队列的「今日到期」按 UTC 日切，也就是本地早上 8 点才换日 —— 午夜到早上 8 点之间，
//     用户看到的是按 UTC 那一天算出来的队列；
//   - 每日汇总按用户设的钟点排，但那个钟点被当成 UTC 的钟点，于是「晚上 8 点」的提醒
//     在本地次日凌晨 4 点才响（实测就是这样，差整整一个偏移）。
//
// 时区名从 WebView 那边问出来（`Intl.DateTimeFormat().resolvedOptions().timeZone`），
// 前端启动时调一次 Set。zoneinfo 那份数据用 time/tzdata **嵌进二进制** —— 安卓上没有
// 系统那份可以读。
package devtz

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	// 把 IANA 时区库嵌进二进制。**这个空导入是整个包能工作的前提**：
	// 没有它，time.LoadLocation 在安卓上找不到任何一份 zoneinfo，只能回落到 UTC ——
	// 那正是本包要解决的问题。
	_ "time/tzdata"
)

var (
	mu sync.RWMutex
	// loc 是当前记着的时区。默认 time.Local —— 桌面上它是对的，安卓上它是 UTC
	// （所以前端必须调一次 Set，见包注释）。
	loc = time.Local
	id  = ""
)

// Set 记住一个 IANA 时区名（如 "Asia/Shanghai"），返回记下的那个。
//
// 名字不认得时**不覆盖**已经记着的那个，并把错报回去 —— 宁可继续用旧的（哪怕是 UTC），
// 也好过把一个坏名字换上去、让后面所有的「今天」都算在一个不存在的时区上。
func Set(zoneID string) (string, error) {
	z := strings.TrimSpace(zoneID)
	if z == "" {
		return "", errors.New("时区名不能为空")
	}

	l, err := time.LoadLocation(z)
	if err != nil {
		return "", fmt.Errorf("载入时区 %q: %w", z, err)
	}

	mu.Lock()
	defer mu.Unlock()
	loc, id = l, z
	return z, nil
}

// Now 是「设备本地的当前时刻」。
//
// 各服务把它当 WithNow 用（原来只有测试会传那个选项）。它们的「今天」都是从
// `now.Location()` 推出来的，所以只要这个时刻带着正确的 Location，
// 那些按本地日切的逻辑一行都不用改。
func Now() time.Time {
	mu.RLock()
	l := loc
	mu.RUnlock()
	return time.Now().In(l)
}

// ID 报告当前记着的时区名。空串表示还没被设过 —— 此时用的是 time.Local，
// 而它在安卓上就是 UTC（见包注释）。
func ID() string {
	mu.RLock()
	defer mu.RUnlock()
	return id
}
