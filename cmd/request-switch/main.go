// Command request-switch is the client CLI: it connects to switchd over the unix
// socket, submits a switch request (mode + reason), streams the daemon's status
// and build/activation logs to stdout, and exits with the switch's exit code.
// LLM agents run this instead of a privileged nixos-rebuild switch. See HANDOVER.md.
package main

func main() {
	// TODO(handover): implement the client per HANDOVER.md:
	//   - connect to SWITCHD_SOCKET_PATH
	//   - send {mode: "sync"|"async", reason}
	//   - stream framed status + log lines to stdout
	//   - propagate the final exit code
}
