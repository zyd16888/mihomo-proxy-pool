package manager

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Airport account counters are byte values, not this process's traffic counters.
type SubscriptionUsage struct {
	UploadBytes    *int64 `json:"uploadBytes"`
	DownloadBytes  *int64 `json:"downloadBytes"`
	TotalBytes     *int64 `json:"totalBytes"`
	Expire         *int64 `json:"expire"`
	UsedBytes      *int64 `json:"usedBytes"`
	RemainingBytes *int64 `json:"remainingBytes"`
	OverageBytes   *int64 `json:"overageBytes"`
	Status         string `json:"status"`
	UpdatedAt      string `json:"updatedAt"`
	CheckedAt      string `json:"checkedAt"`
}

func (u *SubscriptionUsage) calculate() {
	if u.UploadBytes == nil || u.DownloadBytes == nil {
		return
	}
	if *u.UploadBytes > math.MaxInt64-*u.DownloadBytes {
		return
	}
	used := *u.UploadBytes + *u.DownloadBytes
	u.UsedBytes = &used
	if u.TotalBytes != nil && *u.TotalBytes > 0 {
		remaining, overage := max(int64(0), *u.TotalBytes-used), max(int64(0), used-*u.TotalBytes)
		u.RemainingBytes = &remaining
		u.OverageBytes = &overage
	}
}

func ParseSubscriptionUsage(header string) (SubscriptionUsage, error) {
	u := SubscriptionUsage{Status: "current"}
	header = strings.TrimSpace(header)
	if header == "" {
		return u, errors.New("订阅未提供流量信息")
	}
	if len(header) > 8192 {
		return u, errors.New("订阅流量信息过长")
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(header, ";") {
		key, value, hasValue := strings.Cut(strings.TrimSpace(part), "=")
		key = strings.ToLower(strings.TrimSpace(key))
		var field **int64
		switch key {
		case "upload":
			field = &u.UploadBytes
		case "download":
			field = &u.DownloadBytes
		case "total":
			field = &u.TotalBytes
		case "expire":
			field = &u.Expire
		default:
			continue
		}
		if seen[key] || !hasValue {
			return u, errors.New("订阅流量字段重复或格式无效")
		}
		seen[key] = true
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			return u, errors.New("订阅流量字段必须为非负整数")
		}
		if key == "expire" && n > 253402300799 {
			return u, errors.New("订阅到期时间无效")
		}
		*field = &n
	}
	if len(seen) == 0 {
		return u, errors.New("订阅未提供可识别的流量字段")
	}
	if u.UploadBytes != nil && u.DownloadBytes != nil && *u.UploadBytes > math.MaxInt64-*u.DownloadBytes {
		return u, errors.New("订阅流量数值超出范围")
	}
	u.calculate()
	return u, nil
}

// Missing/invalid/failing refreshes only change status, preserving the last valid
// snapshot. The URL condition prevents an old response from updating a new source.
func (s *Store) RecordSubscriptionUsage(ctx context.Context, id, expectedURL string, usage SubscriptionUsage, status string) error {
	if status != "current" && status != "missing" && status != "invalid" && status != "fetch_failed" {
		return errors.New("无效用量状态")
	}
	if status != "current" {
		usage = SubscriptionUsage{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated := ""
	if status == "current" {
		updated = now
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO subscription_usage(subscription_id,upload_bytes,download_bytes,total_bytes,expire,updated_at,checked_at,status)
		SELECT id,?,?,?,?,?,?,? FROM subscriptions WHERE id=? AND url=?
		ON CONFLICT(subscription_id) DO UPDATE SET
		upload_bytes=CASE WHEN excluded.status='current' THEN excluded.upload_bytes ELSE subscription_usage.upload_bytes END,
		download_bytes=CASE WHEN excluded.status='current' THEN excluded.download_bytes ELSE subscription_usage.download_bytes END,
		total_bytes=CASE WHEN excluded.status='current' THEN excluded.total_bytes ELSE subscription_usage.total_bytes END,
		expire=CASE WHEN excluded.status='current' THEN excluded.expire ELSE subscription_usage.expire END,
		updated_at=CASE WHEN excluded.status='current' THEN excluded.updated_at ELSE subscription_usage.updated_at END,
		checked_at=excluded.checked_at,status=excluded.status`, usage.UploadBytes, usage.DownloadBytes, usage.TotalBytes, usage.Expire, updated, now, status, id, expectedURL)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("订阅地址已变化，请重新刷新")
	}
	return nil
}
