// hashgen 开发工具：输出参数的 bcrypt 哈希（用于手动重置管理口令）。
// 用法: go run ./tools/hashgen 'NewPassword'
package main

import (
	"fmt"
	"os"

	"mitmrouter/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hashgen <password>")
		os.Exit(1)
	}
	h, err := store.HashPassword(os.Args[1])
	if err != nil {
		panic(err)
	}
	fmt.Println(h)
}
