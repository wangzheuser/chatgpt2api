package register

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ProbeAuthorize 只执行 platform authorize，用于评估某个邮箱地址能否被上游接受。
// 它不创建账号、不入库，供 cmd/registerprobe 配合外部渠道脚本做渠道评估。
func ProbeAuthorize(ctx context.Context, email, proxy string) (string, error) {
	target := strings.TrimSpace(email)
	if target == "" || !strings.Contains(target, "@") {
		return "", errors.New("邮箱地址不合法")
	}
	sess, err := newSession(proxy, newDeviceID(), nil)
	if err != nil {
		return "", err
	}
	defer sess.close()
	worker, err := newFlow(sess, newSentinelRunner("node"), Mailbox{Address: target}, proxy, func(string, string) {})
	if err != nil {
		return "", err
	}
	if err := worker.authorize(ctx, target); err != nil {
		return "", err
	}
	mode := "challenge"
	if worker.passwordless {
		mode = "passwordless"
	}
	return fmt.Sprintf("authorize_ok mode=%s", mode), nil
}
