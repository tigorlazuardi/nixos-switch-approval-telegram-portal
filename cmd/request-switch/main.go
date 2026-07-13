// Command request-switch submits a fixed-shape switch request to switchd and
// streams daemon frames to stdout. It never sends commands, paths, or flake refs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
)

type request struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

type frame struct {
	Type     string `json:"type"`
	Message  string `json:"message,omitempty"`
	Line     string `json:"line,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func main() {
	mode := flag.String("mode", "sync", "request mode: sync or async")
	socket := flag.String("socket", env("SWITCHD_SOCKET_PATH", "/run/switchd/sock"), "switchd unix socket path")
	flag.Parse()
	if *mode != "sync" && *mode != "async" {
		fmt.Fprintln(os.Stderr, "mode must be sync or async")
		os.Exit(2)
	}
	reason := strings.Join(flag.Args(), " ")
	if reason == "" {
		reason = "manual request-switch"
	}

	conn, err := net.Dial("unix", *socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect switchd: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request{Mode: *mode, Reason: reason}); err != nil {
		fmt.Fprintf(os.Stderr, "send request: %v\n", err)
		os.Exit(1)
	}

	dec := json.NewDecoder(conn)
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			fmt.Fprintf(os.Stderr, "read frame: %v\n", err)
			os.Exit(1)
		}
		switch f.Type {
		case "status":
			if f.Message != "" {
				fmt.Fprintln(os.Stdout, "==> "+f.Message)
			}
		case "log":
			fmt.Fprintln(os.Stdout, f.Line)
		case "error":
			fmt.Fprintln(os.Stderr, "error: "+f.Message)
		case "done":
			if f.Message != "" {
				fmt.Fprintln(os.Stdout, "==> "+f.Message)
			}
			os.Exit(f.ExitCode)
		default:
			fmt.Fprintf(os.Stderr, "unknown frame type %q\n", f.Type)
			os.Exit(2)
		}
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
