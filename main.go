// tonberry-king: Claude Code の複数セッションを localhost の Web 画面で一元監視する。
package main

import (
	"flag"
	"fmt"
	"os"
)

const defaultAddr = "127.0.0.1:7777"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", envOr("TONBERRY_ADDR", defaultAddr), "listen address (loopback only)")
		state := fs.String("state", defaultStatePath(), "state file path")
		claude := fs.String("claude", "claude", "claude command used for launching from the dashboard")
		fs.Parse(os.Args[2:])
		if err := serve(*addr, *state, *claude); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "hook":
		// Claude Code の hook から呼ばれる。どんな場合でも Claude の動作を妨げないよう常に exit 0。
		runHook(envOr("TONBERRY_ADDR", defaultAddr))
	case "url":
		token, err := loadOrCreateToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(dashboardURL(envOr("TONBERRY_ADDR", defaultAddr), token))
	case "hooks-config":
		printHooksConfig()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tonberry-king serve [-addr 127.0.0.1:7777] [-state path] [-claude claude]   start dashboard server
  tonberry-king url                                         print dashboard URL (with access token)
  tonberry-king hook                                        (used from Claude Code hooks; reads JSON from stdin)
  tonberry-king hooks-config                                print hooks snippet for ~/.claude/settings.json`)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
