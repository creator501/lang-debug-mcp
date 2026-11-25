// Command skillgen 由模板展开各语言 skill，并校验产物是否与模板一致。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/langdebug/lang-debug-mcp/internal/skill"
)

func main() {
	root := flag.String("root", ".", "仓库根目录（skills/ 所在目录）")
	check := flag.Bool("check", false, "只校验产物，不写入文件")
	flag.Parse()

	if *check {
		issues, err := skill.Check(*root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skillgen: 校验失败: %v\n", err)
			os.Exit(1)
		}
		if len(issues) > 0 {
			for _, issue := range issues {
				fmt.Println(issue.String())
			}
			fmt.Fprintf(os.Stderr, "skillgen: %d 处不一致\n", len(issues))
			os.Exit(1)
		}
		fmt.Printf("skillgen: 一致性校验通过（%d 个语言 skill）\n", len(skill.Languages()))
		return
	}

	changed, err := skill.Generate(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skillgen: 生成失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("skillgen: 已更新 %d 个文件\n", len(changed))
	for _, path := range changed {
		fmt.Println("  " + path)
	}
}
