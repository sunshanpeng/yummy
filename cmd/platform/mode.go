package main

import (
	"fmt"
	"strings"
)

func parseMode(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "all", args, nil
	}
	switch args[0] {
	case "all", "control", "worker":
		return args[0], args[1:], nil
	default:
		return "", nil, fmt.Errorf("未知子命令 %q，可选值为 all、control、worker", args[0])
	}
}
