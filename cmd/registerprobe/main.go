// Command registerprobe 只做 platform authorize，用于配合外部脚本评估邮箱渠道。
//
// 用法：
//
//	registerprobe -email name@example.com -proxy http://127.0.0.1:7890
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/basketikun/chatgpt2api/internal/register"
)

func main() {
	email := flag.String("email", "", "待验证的邮箱地址")
	proxy := flag.String("proxy", "", "注册出口代理，例如 http://127.0.0.1:7890")
	timeout := flag.Int("timeout", 90, "超时秒数")
	flag.Parse()
	if *email == "" {
		fmt.Fprintln(os.Stderr, "缺少 -email")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()
	detail, err := register.ProbeAuthorize(ctx, *email, *proxy)
	if err != nil {
		fmt.Println("FAIL " + err.Error())
		os.Exit(1)
	}
	fmt.Println("OK " + detail)
}
