package service

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"

	"github.com/microcosm-cc/bluemonday"
	"go.uber.org/zap"
)

// EmailBroadcastService 提供管理员批量发送公告邮件的能力。
//
// 设计要点:
//   - HTTP 入口通过 Send() 立刻创建一条 broadcast 记录后异步发送，避免长时间占用请求 goroutine。
//   - 发送过程对单封邮件失败保持容错，整批的成功 / 失败计数会增量回写到 DB。
//   - HTML 正文使用 bluemonday UGCPolicy 做 sanitize，去除潜在 XSS 风险；纯文本正文会被
//     HTML-escape 并替换换行后再生成最终邮件，保证两种格式的最终 MIME 都是合法的 HTML 邮件。
type EmailBroadcastService struct {
	repo                 EmailBroadcastRepository
	userRepo             UserRepository
	emailService         *EmailService
	settingRepo          SettingRepository
	htmlSanitizer        *bluemonday.Policy
	sendIntervalPerEmail time.Duration

	mu      sync.Mutex
	running map[int64]struct{}
}

// EmailBroadcastSendInput 发送一次广播邮件所需的参数集合 (供 handler 调用)。
type EmailBroadcastSendInput struct {
	Subject          string
	Body             string
	BodyFormat       string
	RecipientsMode   string
	RecipientUserIDs []int64
	CreatedBy        *int64
}

// NewEmailBroadcastService 创建广播邮件服务。
func NewEmailBroadcastService(
	repo EmailBroadcastRepository,
	userRepo UserRepository,
	emailService *EmailService,
	settingRepo SettingRepository,
) *EmailBroadcastService {
	policy := bluemonday.UGCPolicy()
	// 允许 <a target="_blank"> 等公告里常用的属性。
	policy.AllowAttrs("target").OnElements("a")
	policy.RequireNoReferrerOnLinks(true)
	policy.RequireNoFollowOnLinks(true)
	policy.AllowStandardURLs()

	return &EmailBroadcastService{
		repo:                 repo,
		userRepo:             userRepo,
		emailService:         emailService,
		settingRepo:          settingRepo,
		htmlSanitizer:        policy,
		sendIntervalPerEmail: 200 * time.Millisecond,
		running:              make(map[int64]struct{}),
	}
}

// Send 校验输入、解析收件人、立刻持久化 broadcast 记录，然后异步触发批量发送。
// 返回值是已创建的 broadcast (status=pending)。
func (s *EmailBroadcastService) Send(ctx context.Context, input EmailBroadcastSendInput) (*EmailBroadcast, error) {
	subject := strings.TrimSpace(input.Subject)
	body := strings.TrimSpace(input.Body)
	bodyFormat := strings.ToLower(strings.TrimSpace(input.BodyFormat))
	mode := strings.ToLower(strings.TrimSpace(input.RecipientsMode))

	if subject == "" {
		return nil, ErrEmailBroadcastSubjectRequired
	}
	if len([]rune(subject)) > domain.EmailBroadcastSubjectMaxLen {
		return nil, ErrEmailBroadcastSubjectTooLong
	}
	if body == "" {
		return nil, ErrEmailBroadcastBodyRequired
	}
	if len(body) > domain.EmailBroadcastBodyMaxLen {
		return nil, ErrEmailBroadcastBodyTooLong
	}
	switch bodyFormat {
	case EmailBroadcastBodyFormatHTML, EmailBroadcastBodyFormatText:
	case "":
		bodyFormat = EmailBroadcastBodyFormatHTML
	default:
		return nil, ErrEmailBroadcastInvalidBodyFormat
	}
	switch mode {
	case EmailBroadcastRecipientsModeAll, EmailBroadcastRecipientsModeSelected:
	case "":
		mode = EmailBroadcastRecipientsModeSelected
	default:
		return nil, ErrEmailBroadcastInvalidMode
	}

	dedupedIDs := dedupePositiveInt64s(input.RecipientUserIDs)
	if mode == EmailBroadcastRecipientsModeSelected {
		if len(dedupedIDs) == 0 {
			return nil, ErrEmailBroadcastNoRecipients
		}
		if len(dedupedIDs) > domain.EmailBroadcastMaxSelectedRecipients {
			return nil, ErrEmailBroadcastTooManyRecipients
		}
	} else {
		// "all" 模式忽略用户传入的 IDs，避免歧义。
		dedupedIDs = nil
	}

	// 提前校验 SMTP 配置存在 — 没配的话直接返回，不写脏数据。
	smtpConfig, err := s.emailService.GetSMTPConfig(ctx)
	if err != nil {
		return nil, ErrEmailBroadcastEmailNotConfigured
	}
	if smtpConfig == nil || strings.TrimSpace(smtpConfig.Host) == "" {
		return nil, ErrEmailBroadcastEmailNotConfigured
	}

	broadcast := &EmailBroadcast{
		Subject:          subject,
		Body:             body,
		BodyFormat:       bodyFormat,
		RecipientsMode:   mode,
		RecipientUserIDs: dedupedIDs,
		Status:           EmailBroadcastStatusPending,
		CreatedBy:        input.CreatedBy,
	}
	if err := s.repo.Create(ctx, broadcast); err != nil {
		return nil, err
	}

	go s.runBroadcast(broadcast.ID)

	return broadcast, nil
}

// List 查询 broadcast 历史。
func (s *EmailBroadcastService) List(ctx context.Context, params EmailBroadcastListParams) (*EmailBroadcastListResult, error) {
	if params.Page <= 0 {
		params.Page = 1
	}
	if params.PageSize <= 0 || params.PageSize > 100 {
		params.PageSize = 20
	}
	return s.repo.List(ctx, params)
}

// Get 取单条 broadcast 详情。
func (s *EmailBroadcastService) Get(ctx context.Context, id int64) (*EmailBroadcast, error) {
	if id <= 0 {
		return nil, ErrEmailBroadcastNotFound
	}
	return s.repo.GetByID(ctx, id)
}

// runBroadcast 是后台 goroutine 入口，对单次 broadcast 执行解析收件人 + 实际 SMTP 投递 + 状态回写。
// 整个过程使用独立的 context.Background，避免 HTTP 请求结束导致中断。
func (s *EmailBroadcastService) runBroadcast(id int64) {
	if !s.markRunning(id) {
		return
	}
	defer s.unmarkRunning(id)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			logger.L().Error("email_broadcast.panic", zap.Int64("broadcast_id", id), zap.Any("panic", r))
			now := time.Now()
			msg := fmt.Sprintf("panic: %v", r)
			_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
				Status:       ptrStr(EmailBroadcastStatusFailed),
				ErrorMessage: &msg,
				FinishedAt:   &now,
			})
		}
	}()

	broadcast, err := s.repo.GetByID(ctx, id)
	if err != nil || broadcast == nil {
		logger.L().Error("email_broadcast.load_failed", zap.Int64("broadcast_id", id), zap.Error(err))
		return
	}

	smtpConfig, err := s.emailService.GetSMTPConfig(ctx)
	if err != nil || smtpConfig == nil {
		now := time.Now()
		msg := "SMTP config unavailable"
		_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
			Status:       ptrStr(EmailBroadcastStatusFailed),
			ErrorMessage: &msg,
			FinishedAt:   &now,
		})
		return
	}

	emails, err := s.resolveRecipientEmails(ctx, broadcast)
	if err != nil {
		now := time.Now()
		msg := err.Error()
		_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
			Status:       ptrStr(EmailBroadcastStatusFailed),
			ErrorMessage: &msg,
			FinishedAt:   &now,
		})
		return
	}
	if len(emails) == 0 {
		now := time.Now()
		msg := "no valid recipients found"
		zero := 0
		_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
			Status:       ptrStr(EmailBroadcastStatusFailed),
			TotalCount:   &zero,
			ErrorMessage: &msg,
			FinishedAt:   &now,
		})
		return
	}

	htmlBody := s.composeHTMLBody(broadcast.Body, broadcast.BodyFormat)

	started := time.Now()
	total := len(emails)
	zero := 0
	_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
		Status:       ptrStr(EmailBroadcastStatusSending),
		TotalCount:   &total,
		SuccessCount: &zero,
		FailedCount:  &zero,
		StartedAt:    &started,
	})

	success, failed := 0, 0
	for idx, addr := range emails {
		if err := s.emailService.SendEmailWithConfigAndContentType(
			smtpConfig,
			addr,
			broadcast.Subject,
			htmlBody,
			"text/html; charset=UTF-8",
		); err != nil {
			failed++
			logger.L().Warn("email_broadcast.send_failed",
				zap.Int64("broadcast_id", id),
				zap.String("recipient", addr),
				zap.Error(err))
		} else {
			success++
		}

		// Throttle to avoid SMTP rate limits; skip after last message.
		if idx < total-1 && s.sendIntervalPerEmail > 0 {
			select {
			case <-ctx.Done():
				failed += total - idx - 1
				goto done
			case <-time.After(s.sendIntervalPerEmail):
			}
		}
	}

done:
	finished := time.Now()
	finalStatus := EmailBroadcastStatusCompleted
	if success == 0 && failed > 0 {
		finalStatus = EmailBroadcastStatusFailed
	}
	_ = s.repo.UpdateStatus(ctx, id, EmailBroadcastStatusUpdate{
		Status:       ptrStr(finalStatus),
		SuccessCount: &success,
		FailedCount:  &failed,
		FinishedAt:   &finished,
	})

	logger.L().Info("email_broadcast.completed",
		zap.Int64("broadcast_id", id),
		zap.Int("total", total),
		zap.Int("success", success),
		zap.Int("failed", failed),
		zap.String("status", finalStatus))
}

// resolveRecipientEmails 把 broadcast 描述的"全部用户 / 指定 IDs"展开为收件人邮箱列表。
func (s *EmailBroadcastService) resolveRecipientEmails(ctx context.Context, b *EmailBroadcast) ([]string, error) {
	emails := make([]string, 0)
	seen := make(map[string]struct{})

	collect := func(email string) {
		email = strings.TrimSpace(email)
		if email == "" {
			return
		}
		key := strings.ToLower(email)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		emails = append(emails, email)
	}

	switch b.RecipientsMode {
	case EmailBroadcastRecipientsModeAll:
		const pageSize = 500
		page := 1
		for {
			users, pageResult, err := s.userRepo.List(ctx, paginationParams(page, pageSize))
			if err != nil {
				return nil, fmt.Errorf("list users: %w", err)
			}
			for i := range users {
				collect(users[i].Email)
			}
			if pageResult == nil || len(users) < pageSize {
				break
			}
			// 防御性：避免无限循环
			if int64(page*pageSize) >= pageResult.Total {
				break
			}
			page++
		}
	case EmailBroadcastRecipientsModeSelected:
		for _, id := range b.RecipientUserIDs {
			user, err := s.userRepo.GetByID(ctx, id)
			if err != nil {
				logger.L().Warn("email_broadcast.user_lookup_failed",
					zap.Int64("broadcast_id", b.ID),
					zap.Int64("user_id", id),
					zap.Error(err))
				continue
			}
			if user != nil {
				collect(user.Email)
			}
		}
	default:
		return nil, fmt.Errorf("unknown recipients mode: %s", b.RecipientsMode)
	}

	return emails, nil
}

// composeHTMLBody 根据 broadcast 的 body_format 生成最终的 HTML 邮件正文。
// 纯文本会被 HTML-escape 并 <br> 替换换行，HTML 会经过 bluemonday sanitize。
func (s *EmailBroadcastService) composeHTMLBody(body, format string) string {
	switch format {
	case EmailBroadcastBodyFormatText:
		escaped := html.EscapeString(body)
		escaped = strings.ReplaceAll(escaped, "\r\n", "\n")
		escaped = strings.ReplaceAll(escaped, "\n", "<br>")
		return wrapBroadcastHTMLShell(escaped)
	case EmailBroadcastBodyFormatHTML:
		sanitized := s.htmlSanitizer.Sanitize(body)
		return wrapBroadcastHTMLShell(sanitized)
	default:
		return wrapBroadcastHTMLShell(html.EscapeString(body))
	}
}

// wrapBroadcastHTMLShell 给正文包一层最简单的 HTML 容器。
// 故意不引入站点 brand / footer，让公告内容更"管理员发的纯邮件"。
func wrapBroadcastHTMLShell(inner string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"></head><body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;line-height:1.6;color:#1f2937;">
%s
</body></html>`, inner)
}

// markRunning 防止同一个 broadcast 被并发触发两次。
func (s *EmailBroadcastService) markRunning(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[id]; ok {
		return false
	}
	s.running[id] = struct{}{}
	return true
}

func (s *EmailBroadcastService) unmarkRunning(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

func ptrStr(s string) *string { return &s }

func paginationParams(page, pageSize int) pagination.PaginationParams {
	return pagination.PaginationParams{Page: page, PageSize: pageSize}
}

func dedupePositiveInt64s(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
