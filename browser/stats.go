package browser

import (
	"ds2api-browser/config"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type AccountStat struct {
	TotalSuccessRequests int `json:"total_success_requests"`
	LimitTriggers        int `json:"limit_triggers"`
	LoginFailures        int `json:"login_failures"`
	TotalSessions        int `json:"total_sessions"`
}

type StatsManager struct {
	mu       sync.RWMutex
	filePath string
	Data     map[string]*AccountStat
}

func NewStatsManager() *StatsManager {
	exePath, _ := os.Executable()
	filePath := filepath.Join(filepath.Dir(exePath), "account_stats.json")
	sm := &StatsManager{
		filePath: filePath,
		Data:     make(map[string]*AccountStat),
	}
	sm.load()
	// 启动时清零所有账号的登录失败计数
	// 原因：login_failures 跨会话持久化，但网络波动是临时性的。前次会话的网络波动
	// 会导致账号被永久禁用，永远没机会重试。清零后真正有问题的账号（密码错误/封禁）
	// 会在下次登录时重新被检测并禁用，不影响安全性。
	sm.resetLoginFailures()
	return sm
}

func (sm *StatsManager) load() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	file, err := os.ReadFile(sm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("[stats] load failed: %v", err)
		return
	}

	if err := json.Unmarshal(file, &sm.Data); err != nil {
		log.Printf("[stats] unmarshal failed: %v", err)
	}
}

func (sm *StatsManager) save() {
	// Assumes lock is already held by caller
	data, err := json.MarshalIndent(sm.Data, "", "  ")
	if err != nil {
		log.Printf("[stats] marshal failed: %v", err)
		return
	}

	if err := os.WriteFile(sm.filePath, data, 0644); err != nil {
		log.Printf("[stats] write file failed: %v", err)
	}
}

func (sm *StatsManager) RecordLimitTrigger(email string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	stat := sm.getOrCreateStat(email)
	stat.LimitTriggers++
	sm.save()
}

func (sm *StatsManager) RecordLoginFailure(email string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	stat := sm.getOrCreateStat(email)
	stat.LoginFailures++
	sm.save()
}

// RecordLoginSuccess 登录成功后清零登录失败计数
// 之前因网络波动导致的失败记录会被清除，避免临时故障累积导致账号被误禁用
func (sm *StatsManager) RecordLoginSuccess(email string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	stat := sm.getOrCreateStat(email)
	if stat.LoginFailures > 0 {
		log.Printf("[stats] 账号 %s 登录成功，清零登录失败计数（原值=%d）", email, stat.LoginFailures)
		stat.LoginFailures = 0
		sm.save()
	}
}

func (sm *StatsManager) RecordSessionEnd(email string, successRequests int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	stat := sm.getOrCreateStat(email)
	stat.TotalSuccessRequests += successRequests
	stat.TotalSessions++
	sm.save()
}

func (sm *StatsManager) getOrCreateStat(email string) *AccountStat {
	if stat, ok := sm.Data[email]; ok {
		return stat
	}
	stat := &AccountStat{}
	sm.Data[email] = stat
	return stat
}

func (sm *StatsManager) GetStatsJSON() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	data, _ := json.MarshalIndent(sm.Data, "", "  ")
	return string(data)
}

func (sm *StatsManager) GetSortedIndices(accounts []config.Account) []int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	type accountScore struct {
		index int
		score float64
	}

	// [账号质量管理] 登录失败次数 > 0 的账号直接禁用，不参与排序和切换
	// 禁用账号会在 LogDisabledAccounts 中报告，由用户决定是否手动清理 stats 文件恢复
	var scores []accountScore
	for i, acc := range accounts {
		stat := sm.Data[acc.Email]
		if stat != nil && stat.LoginFailures > 0 {
			continue
		}
		var score float64
		if stat != nil {
			// 质量分 = 成功请求数 / (1 + 限制触发次数)
			// 登录失败账号已排除，分母不再包含 LoginFailures
			score = float64(stat.TotalSuccessRequests) / float64(1+stat.LimitTriggers)
		}
		scores = append(scores, accountScore{index: i, score: score})
	}

	// 按分数降序排列
	sort.SliceStable(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	indices := make([]int, len(scores))
	for i, s := range scores {
		indices[i] = s.index
	}
	return indices
}

// LogDisabledAccounts 在日志中列出因登录失败被禁用的账号，提醒用户处理
func (sm *StatsManager) LogDisabledAccounts(accounts []config.Account) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var disabled []string
	availableCount := 0
	for _, acc := range accounts {
		stat := sm.Data[acc.Email]
		if stat != nil && stat.LoginFailures > 0 {
			disabled = append(disabled, fmt.Sprintf("  %s (登录失败%d次, 成功%d次, 限制%d次)",
				acc.Email, stat.LoginFailures, stat.TotalSuccessRequests, stat.LimitTriggers))
		} else {
			availableCount++
		}
	}

	if len(disabled) > 0 {
		log.Printf("[stats] ⚠️ 以下账号因登录失败已被禁用:\n%s", strings.Join(disabled, "\n"))
		log.Printf("[stats] 可用账号: %d个, 禁用账号: %d个", availableCount, len(disabled))
	} else {
		log.Printf("[stats] 所有账号状态正常, 可用账号: %d个", availableCount)
	}
}

// resetLoginFailures 启动时清零所有账号的登录失败计数
// 前次会话的网络波动导致的失败不应永久禁用账号，清零后真正有问题的账号会在下次登录时重新被检测
func (sm *StatsManager) resetLoginFailures() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cleared := 0
	for email, stat := range sm.Data {
		if stat.LoginFailures > 0 {
			log.Printf("[stats] 清零账号 %s 的登录失败计数（原值=%d, 成功=%d, 限制=%d）",
				email, stat.LoginFailures, stat.TotalSuccessRequests, stat.LimitTriggers)
			stat.LoginFailures = 0
			cleared++
		}
	}
	if cleared > 0 {
		sm.save()
		log.Printf("[stats] 已清零 %d 个账号的登录失败计数", cleared)
	}
}
