// tonberry-king: a localhost dashboard to monitor and control multiple Claude Code sessions.
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
		// Called from Claude Code hooks. Always exits 0 so it never interferes with Claude.
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
