// Command switchd is the switch-approval daemon: it listens on a unix socket for
// switch requests, asks the operator to approve via Telegram, and — on approval —
// runs the fixed nixos-rebuild switch through a scoped-sudo command, streaming
// logs back to the caller. See HANDOVER.md for the full brief and the security
// invariants (which are non-negotiable).
package main

func main() {
	// TODO(handover): implement the daemon per HANDOVER.md:
	//   - unix socket server (SWITCHD_SOCKET_PATH), group-restricted
	//   - Telegram long-poll getUpdates + inline Approve/Reject keyboard
	//   - build-then-ask approval flow, nonce-bound callbacks
	//   - scoped-sudo exec (argv vector, never a shell) + log streaming
	//   - sync + async request modes
	//   - OpenTelemetry (OTLP to the host Alloy)
}
