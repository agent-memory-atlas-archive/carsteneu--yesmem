package daemon

import (
	"strings"

	"github.com/carsteneu/yesmem/internal/storage"
)

// daemonSenderPrefix marks a channel message as daemon-originated. A daemon
// sender names no session: it never resolves through resolveSessionID, so a
// notice can no longer be attributed to whichever session wrote last.
//
// A daemon address is send-only. Nothing polls it — the heartbeat walks running
// agents and their ChannelAddresses, and ResolveChannelTarget passes "daemon:*"
// through as a named channel — so a reply addressed to it would be stored and
// never delivered. Daemon notices are msg_type "status", for which the proxy's
// channel directive is "do not reply". The heartbeat therefore logs an
// undeliverable daemon-originated notice instead of re-addressing it.
//
// The prefix is a routing label, not a credential: handleSendTo reads the sender
// from the request, and the daemon socket is a same-UID unix socket without an
// internal/external marker, so any client of that socket can claim a daemon
// address. The blast radius is bounded — a claimed daemon sender suppresses only
// that caller's implicit ACK and its ACK-on-ACK check, and it expands into no
// session's address set, so it cannot deliver, read or delete another session's
// messages. That socket can already address any session directly, which is the
// wider pre-existing exposure this loop did not change.
const daemonSenderPrefix = "daemon:"

// daemonSender builds the channel address of a daemon subsystem. The two in use
// are daemonSender("yesloop") for the guard notices sent through
// notifyOrchestrator and daemonSender("heartbeat") for the crash/status notices
// in heartbeat.go. The address is a named channel: it is stored and matched
// verbatim, and it is never expanded into a session's address set.
func daemonSender(subsystem string) string {
	return daemonSenderPrefix + subsystem
}

// isDaemonSender reports whether an address belongs to the daemon rather than to
// a session. handleSendTo uses it to skip the implicit ACK — there is no inbox
// behind a daemon address, and acknowledging one would mark another session's
// messages read.
func isDaemonSender(addr string) bool {
	return strings.HasPrefix(addr, daemonSenderPrefix)
}

// implicitAckIdentity returns the identity whose incoming messages a send_to
// may mark read, or "" when none may be. The ack must only ever touch the
// caller's own inbox:
//   - a daemon sender has no inbox;
//   - a sender the caller derived from nothing is not acknowledged: the sender
//     came from the global active-session guess, which names whichever session
//     wrote last;
//   - an explicit named channel ("orchestrator", "telegram-bridge-…") keeps the
//     legacy behaviour — named channels are mailboxes of their own, not sessions;
//   - an explicit id-like sender is acknowledged only when it is the caller's
//     proven identity, so a free parameter cannot drain another session's inbox.
//
// caller is the identity provenSessionID derived from the request. The
// classification runs on the canonical form, because that is the form the ack
// expands (ChannelAddresses trims and canonicalises): a padded id must not pass
// as a named channel. This guards against mistakes, not against a hostile local
// client — every proof source (_session_id, _caller_pid, _cwd) is set by the
// client on a same-UID socket.
func (h *Handler) implicitAckIdentity(params map[string]any, sender, caller string) string {
	if sender == "" || sender == "unknown" || isDaemonSender(storage.CanonicalAddress(sender)) {
		return ""
	}
	explicit, _ := params["sender"].(string)
	if explicit == "" {
		explicit, _ = params["session_id"].(string)
	}
	if explicit == "" {
		explicit, _ = params["_sender"].(string)
	}
	if explicit == "" {
		if caller == "" || caller != sender {
			return ""
		}
		return sender
	}
	want := storage.CanonicalAddress(explicit)
	if !storage.LooksLikeChannelID(want) {
		return sender
	}
	if caller == "" {
		return ""
	}
	if storage.CanonicalAddress(caller) == want {
		return sender
	}
	for _, addr := range h.store.ChannelAddresses(caller) {
		if addr == want {
			return sender
		}
	}
	return ""
}
