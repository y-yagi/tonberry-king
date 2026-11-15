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
		remoteAddr := fs.String("remote-addr", os.Getenv("TONBERRY_REMOTE_ADDR"), "remote (phone) listener address, loopback only; expose it with tailscale serve")
		remoteHost := fs.String("remote-host", os.Getenv("TONBERRY_REMOTE_HOST"), "host name the remote listener accepts, e.g. pc.tailnet.ts.net")
		fs.Parse(os.Args[2:])
		cfg := serveConfig{addr: *addr, statePath: *state, claudeBin: *claude, remoteAddr: *remoteAddr, remoteHost: *remoteHost}
		if err := serve(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "hook":
		// Called from Claude Code hooks. Always exits 0 so it never interferes with Claude.
		runHook(envOr("TONBERRY_ADDR", defaultAddr))
	case "url":
		fs := flag.NewFlagSet("url", flag.ExitOnError)
		remote := fs.Bool("remote", false, "print the URL for the phone (remote listener)")
		remoteHost := fs.String("remote-host", os.Getenv("TONBERRY_REMOTE_HOST"), "host name of the remote listener")
		fs.Parse(os.Args[2:])
		if *remote {
			if *remoteHost == "" {
				fmt.Fprintln(os.Stderr, "-remote-host (or TONBERRY_REMOTE_HOST) is required with -remote")
				os.Exit(2)
			}
			token, err := loadOrCreateTokenAt(remoteTokenPath())
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(remoteDashboardURL(*remoteHost, token))
			return
		}
		token, err := loadOrCreateToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(dashboardURL(envOr("TONBERRY_ADDR", defaultAddr), token))
	case "rotate-remote-token":
		if _, err := rotateRemoteToken(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "remote token rotated; the old one no longer works. Run `tonberry-king url -remote` for the new URL.")
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
  tonberry-king serve ... -remote-addr 127.0.0.1:7778 -remote-host pc.tailnet.ts.net
                                                            also serve the phone listener (behind tailscale serve)
  tonberry-king url [-remote [-remote-host host]]           print dashboard URL (with access token)
  tonberry-king rotate-remote-token                         revoke the phone token and create a new one
  tonberry-king hook                                        (used from Claude Code hooks; reads JSON from stdin)
  tonberry-king hooks-config                                print hooks snippet for ~/.claude/settings.json`)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
