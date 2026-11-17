package register

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/google/uuid"
)

// Service 是注册机编排层，对应 dev 的 RegisterService。
// 所有运行状态写入 data/register.json，不修改上游任何模型。
type Service struct {
	config   *config.Config
	accounts *service.AccountService
	proxySvc *service.ProxyService
	store    *store
	runner   *sentinelRunner
	clear    clearanceProvider
	pool     *proxyPool

	mu      sync.RWMutex
	cfg     Config
	logs    []LogEntry
	running bool
	cancel  context.CancelFunc
	started time.Time
}

// NewService 构造注册机服务。
func NewService(cfg *config.Config, accounts *service.AccountService, proxySvc *service.ProxyService) *Service {
	dataDir := strings.TrimSpace(os.Getenv("REGISTER_DATA_DIR"))
	if dataDir == "" {
		dataDir = "data"
	}
	nodeBin := strings.TrimSpace(os.Getenv("REGISTER_NODE_BIN"))
	if nodeBin == "" {
		nodeBin = "node"
	}
	svc := &Service{
		config:   cfg,
		accounts: accounts,
		proxySvc: proxySvc,
		store:    newStore(dataDir),
		runner:   newSentinelRunner(nodeBin),
		clear:    proxyClearance{svc: proxySvc},
		pool:     newProxyPool(),
	}
	outlookStatePath = filepath.Join(dataDir, "outlook_token_used.json")
	svc.cfg = svc.store.load()
	svc.cfg.Proxy = strings.TrimSpace(svc.cfg.Proxy)
	return svc
}

// proxyClearance 把 service.ProxyService 的 clearance 能力适配成注册机接口。
type proxyClearance struct{ svc *service.ProxyService }

func (p proxyClearance) Clearance(ctx context.Context, targetURL string) (map[string]string, string, error) {
	if p.svc == nil {
		return nil, "", nil
	}
	host := hostOf(targetURL)
	bundle := p.svc.CachedClearance(host)
	if len(bundle.Cookies) == 0 || time.Now().After(bundle.ExpiresAt) {
		refreshed, err := p.svc.RefreshClearance(ctx, targetURL)
		if err != nil {
			return nil, "", err
		}
		bundle = refreshed
	}
	return bundle.Cookies, bundle.UserAgent, nil
}

func hostOf(targetURL string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(targetURL, "https://"), "http://")
	if index := strings.IndexByte(trimmed, '/'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// recorder 给邮箱层提供日志与统计入口。
type recorder struct{ svc *Service }

func (r *recorder) log(text, level string) {
	if r == nil || r.svc == nil {
		return
	}
	r.svc.appendLog(text, level)
}

func (r *recorder) bucket(kind, key string, success bool, err error) {
	if r == nil || r.svc == nil {
		return
	}
	r.svc.bumpBucket(kind, key, success, err)
}

// Snapshot 返回对外可见配置与运行状态。
// proxyStatusInline 返回代理池健康快照。
func (s *Service) proxyStatusInline() []map[string]any {
	if s.pool == nil {
		return nil
	}
	return s.pool.status(s.cfg.Proxies, s.cfg.Proxy)
}

func (s *Service) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]any{"register": map[string]any{
		"mail":             s.mailSnapshotLocked(),
		"proxy":            s.cfg.Proxy,
		"proxies":          s.cfg.Proxies,
		"proxy_status":     s.proxyStatusInline(),
		"total":            s.cfg.Total,
		"threads":          s.cfg.Threads,
		"mode":             s.cfg.Mode,
		"target_quota":     s.cfg.TargetQuota,
		"target_available": s.cfg.TargetAvailable,
		"check_interval":   s.cfg.CheckInterval,
		"enabled":          s.cfg.Enabled,
		"stats":            s.cfg.Stats,
		"logs":             append([]LogEntry(nil), s.logs...),
	}}
}

// UpdateRequest 是 POST /api/register 的补丁结构，nil 字段表示不修改。
type UpdateRequest struct {
	Mail            *MailConfig `json:"mail"`
	Proxy           *string     `json:"proxy"`
	Proxies         *[]string   `json:"proxies"`
	Total           *int        `json:"total"`
	Threads         *int        `json:"threads"`
	Mode            *string     `json:"mode"`
	TargetQuota     *int        `json:"target_quota"`
	TargetAvailable *int        `json:"target_available"`
	CheckInterval   *int        `json:"check_interval"`
}

// Update 应用配置补丁并持久化。
func (s *Service) Update(patch UpdateRequest) map[string]any {
	s.mu.Lock()
	if patch.Mail != nil {
		merged := *patch.Mail
		for index := range merged.Providers {
			provider := &merged.Providers[index]
			if provider.Type != "outlook_token" {
				continue
			}
			oldText := ""
			if index < len(s.cfg.Mail.Providers) && s.cfg.Mail.Providers[index].Type == "outlook_token" {
				oldText = s.cfg.Mail.Providers[index].Mailboxes
			}
			if oldText != "" || provider.Mailboxes != "" {
				provider.Mailboxes = mergeOutlookCredentials(oldText, provider.Mailboxes)
			}
			provider.MailboxesCount = 0
			provider.MailboxesPreview = nil
			provider.MailboxesStats = nil
		}
		s.cfg.Mail = merged
	}
	if patch.Proxy != nil {
		s.cfg.Proxy = strings.TrimSpace(*patch.Proxy)
	}
	if patch.Proxies != nil {
		s.cfg.Proxies = *patch.Proxies
	}
	if patch.Total != nil {
		s.cfg.Total = *patch.Total
	}
	if patch.Threads != nil {
		s.cfg.Threads = *patch.Threads
	}
	if patch.Mode != nil {
		s.cfg.Mode = *patch.Mode
	}
	if patch.TargetQuota != nil {
		s.cfg.TargetQuota = *patch.TargetQuota
	}
	if patch.TargetAvailable != nil {
		s.cfg.TargetAvailable = *patch.TargetAvailable
	}
	if patch.CheckInterval != nil {
		s.cfg.CheckInterval = *patch.CheckInterval
	}
	s.cfg.normalize()
	snapshot := s.cfg
	s.mu.Unlock()
	if err := s.store.save(snapshot); err != nil {
		s.appendLog("保存注册配置失败: "+err.Error(), "red")
	}
	return s.Snapshot()
}

// Start 启动注册任务，已在运行则只标记 enabled。
func (s *Service) Start() map[string]any {
	s.mu.Lock()
	if s.running {
		s.cfg.Enabled = true
		snapshot := s.cfg
		s.mu.Unlock()
		_ = s.store.save(snapshot)
		return s.Snapshot()
	}
	s.cfg.Enabled = true
	s.cfg.Stats = Stats{
		JobID:             strings.ReplaceAll(uuid.NewString(), "-", ""),
		Threads:           s.cfg.Threads,
		MailProviderStats: map[string]BucketStats{},
		MailDomainStats:   map[string]BucketStats{},
		StartedAt:         nowISO(),
		UpdatedAt:         nowISO(),
	}
	s.logs = nil
	s.running = true
	s.started = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	snapshot := s.cfg
	s.mu.Unlock()
	_ = s.store.save(snapshot)
	s.appendLog(fmt.Sprintf("注册任务启动，模式=%s，线程数=%d", s.cfg.Mode, s.cfg.Threads), "yellow")
	go s.run(ctx)
	return s.Snapshot()
}

// Stop 只置 enabled=false，等待当前任务收尾（与 dev 语义一致）。
func (s *Service) Stop() map[string]any {
	s.mu.Lock()
	s.cfg.Enabled = false
	s.cfg.Stats.UpdatedAt = nowISO()
	snapshot := s.cfg
	s.mu.Unlock()
	_ = s.store.save(snapshot)
	s.appendLog("已请求停止注册任务，正在等待当前运行任务结束", "yellow")
	return s.Snapshot()
}

// Reset 清空统计与日志，不终止任务。
func (s *Service) Reset() map[string]any {
	s.mu.Lock()
	s.logs = nil
	s.cfg.Stats = Stats{
		Threads:           s.cfg.Threads,
		MailProviderStats: map[string]BucketStats{},
		MailDomainStats:   map[string]BucketStats{},
		UpdatedAt:         nowISO(),
	}
	snapshot := s.cfg
	s.mu.Unlock()
	_ = s.store.save(snapshot)
	return s.Snapshot()
}

// ResetOutlookPool 重置 outlook 凭据池状态，语义与 dev 一致。
func (s *Service) ResetOutlookPool(scope string) map[string]any {
	scope = strings.ToLower(strings.TrimSpace(scope))
	switch scope {
	case "unused":
		s.mu.Lock()
		removed := 0
		for index := range s.cfg.Mail.Providers {
			provider := &s.cfg.Mail.Providers[index]
			if provider.Type != "outlook_token" {
				continue
			}
			kept, count := pruneOutlookUnused(parseOutlookCredentials(provider.Mailboxes))
			provider.Mailboxes = serializeOutlookCredentials(kept)
			provider.MailboxesCount = 0
			provider.MailboxesPreview = nil
			provider.MailboxesStats = nil
			removed += count
		}
		snapshot := s.cfg
		s.mu.Unlock()
		_ = s.store.save(snapshot)
		s.appendLog(fmt.Sprintf("已清空 Outlook 邮箱池未使用邮箱，移除 %d 个", removed), "yellow")
	case "failed":
		cleared := resetOutlookState("failed")
		s.appendLog(fmt.Sprintf("已重置 Outlook 邮箱池状态（范围=仅失败/占用），清除 %d 条记录", cleared), "yellow")
	default:
		cleared := resetOutlookState("all")
		s.appendLog(fmt.Sprintf("已重置 Outlook 邮箱池状态（范围=全部），清除 %d 条记录", cleared), "yellow")
	}
	return s.Snapshot()
}

// mailSnapshotLocked 返回脱敏后的邮箱配置：outlook 凭据只暴露数量、掩码预览与状态统计。
func (s *Service) mailSnapshotLocked() MailConfig {
	mail := s.cfg.Mail
	providers := make([]MailProviderConfig, len(mail.Providers))
	for index, item := range mail.Providers {
		sanitized := item
		if sanitized.Type == "outlook_token" {
			credentials := parseOutlookCredentials(sanitized.Mailboxes)
			sanitized.Mailboxes = ""
			sanitized.MailboxesCount = len(credentials)
			preview := make([]string, 0, len(credentials))
			for _, credential := range credentials {
				preview = append(preview, maskOutlookEmail(credential.Email))
			}
			sanitized.MailboxesPreview = preview
			sanitized.MailboxesStats = outlookPoolStats(credentials)
		}
		providers[index] = sanitized
	}
	mail.Providers = providers
	return mail
}

// markMailboxResult 注册结束后更新 outlook 凭据状态。
func (s *Service) markMailboxResult(mailbox Mailbox, success bool, err error) {
	if mailbox.Provider != "outlook_token" {
		return
	}
	if success {
		setOutlookState(mailbox.Address, outlookStateUsed, "")
		return
	}
	if err != nil && isOutlookTokenError(err) {
		setOutlookState(mailbox.Address, outlookStateTokenInvalid, truncate(err.Error(), 300))
		return
	}
	reason := ""
	if err != nil {
		reason = truncate(err.Error(), 300)
	}
	setOutlookState(mailbox.Address, outlookStateFailed, reason)
}

func (s *Service) appendLog(text, level string) {
	if strings.TrimSpace(level) == "" {
		level = "info"
	}
	s.mu.Lock()
	s.logs = append(s.logs, LogEntry{Time: nowISO(), Text: text, Level: level})
	if len(s.logs) > 300 {
		s.logs = s.logs[len(s.logs)-300:]
	}
	s.mu.Unlock()
}

func (s *Service) bumpBucket(kind, key string, success bool, err error) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.cfg.Stats.MailProviderStats
	if kind == "mail_domain_stats" {
		target = s.cfg.Stats.MailDomainStats
	}
	if target == nil {
		target = map[string]BucketStats{}
		if kind == "mail_domain_stats" {
			s.cfg.Stats.MailDomainStats = target
		} else {
			s.cfg.Stats.MailProviderStats = target
		}
	}
	bucket := target[key]
	if success {
		bucket.Success++
	} else {
		bucket.Fail++
		if err != nil {
			bucket.LastErr = truncate(err.Error(), 200)
		}
	}
	target[key] = bucket
}

// currentConfig 返回配置副本。
func (s *Service) currentConfig() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// proxyURL 返回单个回落代理（全局配置优先于内置默认）。
func (s *Service) proxyURL(cfg Config) string {
	if value := strings.TrimSpace(cfg.Proxy); value != "" {
		return value
	}
	if s.config != nil {
		return strings.TrimSpace(s.config.Proxy)
	}
	return ""
}

// pickProxy 从批量池中轮询一个可用代理，池为空时回落到单个代理。
func (s *Service) pickProxy(cfg Config) string {
	return s.pool.pick(cfg.Proxies, s.proxyURL(cfg))
}

// poolMetrics 读取号池额度与可用账号数。
func (s *Service) poolMetrics(ctx context.Context) (int, int) {
	if s.accounts == nil {
		return 0, 0
	}
	summary, err := s.accounts.Summary(ctx)
	if err != nil {
		return 0, 0
	}
	return int(summary["quota"]), int(summary["active"])
}

// targetReached 判断是否达到停止条件。
func (s *Service) targetReached(ctx context.Context, cfg Config, submitted int) bool {
	quota, available := s.poolMetrics(ctx)
	s.mu.Lock()
	s.cfg.Stats.CurrentQuota = quota
	s.cfg.Stats.CurrentAvailable = available
	s.mu.Unlock()
	switch cfg.Mode {
	case ModeQuota:
		reached := quota >= cfg.TargetQuota
		s.appendLog(fmt.Sprintf("检查号池：当前可用账号=%d，当前剩余额度=%d，目标额度=%d，%s",
			available, quota, cfg.TargetQuota, ternary(reached, "跳过注册", "继续注册")), "yellow")
		return reached
	case ModeAvailable:
		reached := available >= cfg.TargetAvailable
		s.appendLog(fmt.Sprintf("检查号池：当前可用账号=%d，目标账号=%d，当前剩余额度=%d，%s",
			available, cfg.TargetAvailable, quota, ternary(reached, "跳过注册", "继续注册")), "yellow")
		return reached
	default:
		return submitted >= cfg.Total
	}
}

func ternary(condition bool, yes, no string) string {
	if condition {
		return yes
	}
	return no
}

// run 是 worker pool 主循环。
func (s *Service) run(ctx context.Context) {
	cfg := s.currentConfig()
	threads := cfg.Threads
	if threads < 1 {
		threads = 1
	}
	sem := make(chan struct{}, threads)
	var wg sync.WaitGroup
	submitted := 0
	done, success, fail := 0, 0, 0
	stopped := false
	for !stopped {
		current := s.currentConfig()
		if !current.Enabled {
			break
		}
		if s.targetReached(ctx, current, submitted) {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			stopped = true
			continue
		}
		submitted++
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ok := s.worker(ctx, index)
			<-sem
			s.mu.Lock()
			done++
			if ok {
				success++
			} else {
				fail++
			}
			s.cfg.Stats.Done = done
			s.cfg.Stats.Success = success
			s.cfg.Stats.Fail = fail
			s.cfg.Stats.Running = len(sem)
			s.cfg.Stats.UpdatedAt = nowISO()
			if elapsed := time.Since(s.started).Seconds(); elapsed > 0 {
				s.cfg.Stats.ElapsedSeconds = float64(int(elapsed))
				if success > 0 {
					s.cfg.Stats.AvgSeconds = float64(int(elapsed)) / float64(success)
				}
				if success+fail > 0 {
					s.cfg.Stats.SuccessRate = float64(int(float64(success)*1000/float64(success+fail))) / 10
				}
			}
			s.mu.Unlock()
		}(submitted)
		s.mu.Lock()
		s.cfg.Stats.Running = len(sem)
		s.cfg.Stats.UpdatedAt = nowISO()
		s.mu.Unlock()
		if current.Mode != ModeTotal && len(sem) == 0 {
			time.Sleep(time.Duration(maxInt(current.CheckInterval, 1)) * time.Second)
		}
	}
	wg.Wait()
	s.mu.Lock()
	s.cfg.Enabled = false
	s.cfg.Stats.Running = 0
	s.cfg.Stats.FinishedAt = nowISO()
	s.cfg.Stats.UpdatedAt = nowISO()
	s.running = false
	snapshot := s.cfg
	s.mu.Unlock()
	_ = s.store.save(snapshot)
	s.appendLog(fmt.Sprintf("注册任务结束，成功%d，失败%d", success, fail), "yellow")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// worker 执行单个注册任务：建邮箱 → 走授权流程 → 入库。
func (s *Service) worker(ctx context.Context, index int) bool {
	cfg := s.currentConfig()
	started := time.Now()
	// rawProxy 可能含 {uuid} 模板：健康统计按模板记录，实际连接用渲染后的会话代理。
	rawProxy := s.pickProxy(cfg)
	proxy := renderProxyTemplate(rawProxy)
	rec := &recorder{svc: s}
	// 邮箱 API 默认可用注册代理，但部分渠道会因数据中心 IP 直接断开，
	// 关闭 api_use_register_proxy 后邮箱走本机直连、注册仍走代理。
	mailProxy := ""
	if cfg.Mail.APIUseRegisterProxy {
		mailProxy = proxy
	}
	hub := newMailHub(cfg.Mail, mailProxy, rec)
	defer hub.close()

	mailbox, err := hub.create(ctx, "")
	if err != nil {
		s.appendLog(fmt.Sprintf("任务%d 创建邮箱失败: %v", index, err), "red")
		return false
	}
	sess, err := newSession(proxy, newDeviceID(), s.clear)
	if err != nil {
		s.appendLog(fmt.Sprintf("任务%d 初始化会话失败: %v", index, err), "red")
		return false
	}
	defer sess.close()
	worker, err := newFlow(sess, s.runner, mailbox, proxy, s.appendLog)
	if err != nil {
		s.appendLog(fmt.Sprintf("任务%d 初始化流程失败: %v", index, err), "red")
		return false
	}
	worker.codeWaiter = hub.waitCode
	result, err := worker.run(ctx)
	if err != nil {
		if isProxyFailure(err) {
			s.pool.markFailure(rawProxy)
			s.appendLog(fmt.Sprintf("任务%d 出口代理判定失败并进入冷却: %s", index, redactProxy(proxy)), "yellow")
		}
		s.markMailboxResult(mailbox, false, err)
		s.appendLog(fmt.Sprintf("任务%d 注册失败，耗时%.1fs，原因: %v", index, time.Since(started).Seconds(), err), "red")
		return false
	}
	s.pool.markSuccess(rawProxy)
	s.markMailboxResult(mailbox, true, nil)
	if s.accounts != nil {
		email := result.Email
		item := repository.Account{
			AccessToken:  result.AccessToken,
			RefreshToken: result.RefreshToken,
			IDToken:      result.IDToken,
			Email:        &email,
			CreatedAt:    result.CreatedAt,
		}
		added, skipped, refreshed, refreshErrors, addErr := s.accounts.Add(ctx, []repository.Account{item})
		if addErr != nil {
			s.appendLog(fmt.Sprintf("任务%d 账号已注册但入库失败: %v", index, addErr), "red")
			return false
		}
		s.appendLog(fmt.Sprintf("任务%d 入库完成 added=%d updated=%d refreshed=%d", index, added, skipped, refreshed), "")
		if len(refreshErrors) > 0 {
			s.appendLog(fmt.Sprintf("任务%d 账号已保存，刷新状态暂未成功，稍后可重试: %v", index, refreshErrors), "yellow")
		}
	}
	s.appendLog(fmt.Sprintf("%s 注册成功，本次耗时%.1fs", result.Email, time.Since(started).Seconds()), "green")
	return true
}

// ensure errors import is used even when future paths drop it.
var _ = errors.New
