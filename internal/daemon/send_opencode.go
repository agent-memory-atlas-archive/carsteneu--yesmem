package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/channeltext"
	"github.com/carsteneu/yesmem/internal/opencode"
)

// opencodeAddressPrefix namespaces an opencode session in a channel address.
// A message stored under such an address is readable by that session's channel
// consumer, which is why the same address is what the API delivery pushes to.
const opencodeAddressPrefix = "opencode:"

// isOpencodeAddress reports whether a canonical channel address names an
// opencode session — that is, whether there is an inbox API to push to. A
// named channel and every other backend have none.
//
// The id is also held to the characters a session id can contain, because it
// becomes a path segment on the service. An address that carries a slash, an
// escape or a control character is not a session of this installation, and
// passing it on would leave the pushed session to the remote router's own
// decoding — and would let the self-wake comparison below be sidestepped.
func isOpencodeAddress(addr string) bool {
	rest, ok := strings.CutPrefix(addr, opencodeAddressPrefix)
	if !ok || !strings.HasPrefix(rest, "ses_") {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			continue
		}
		return false
	}
	return true
}

// sendDeliveryTimeout bounds one delivery attempt. It is deliberately shorter
// than the client's own timeout, because the daemon also reaches this path from
// the shared heartbeat goroutine (a daemon-internal report to the orchestrator)
// and a slow service must not hold that goroutine. A queued turn is
// acknowledged as soon as it sits in the session's inbox, not when the session
// has processed it, so a healthy call returns in milliseconds.
//
// The deadline covers the request. The service lookup in front of it is bounded
// separately, and less tightly than it looks: FirstLiveService gives one shared
// short budget to every registered service, so a stale registration that
// accepts a connection and then stalls can consume it and leave the lookup
// empty — which degrades the send to the stored path rather than failing it.
const sendDeliveryTimeout = 3 * time.Second

// deliveryOutcome is what became of one delivery attempt, as reported back to
// the caller.
type deliveryOutcome string

const (
	// deliveryStored: the message waits for the target's own next request.
	deliveryStored deliveryOutcome = "stored"
	// deliveryAPI: the service took the turn into the target's inbox.
	deliveryAPI deliveryOutcome = "api"
	// deliveryUnconfirmed: the push was attempted and the service neither
	// took nor refused it in time. The message stays claimed — it will not be
	// delivered a second time either — so the target may or may not have it.
	deliveryUnconfirmed deliveryOutcome = "unconfirmed"
)

// channelDelivery is one stored channel message to push into an opencode
// session's inbox.
type channelDelivery struct {
	// Address is the canonical address the message was stored under.
	Address string
	// Sender is the label the receiving agent sees in the text and metadata.
	Sender string
	// Caller is the identity of the session that issued the send, as far as it
	// could be established, or "" when it could not — see deliveryCaller. It is
	// compared against Address so that no session is woken by its own message.
	// It is not the same value as Sender on purpose: Sender falls back to a
	// guess for the message text, and a guess must not decide whether a
	// delivery happens.
	Caller    string
	Content   string
	MsgType   string
	MessageID int64
}

// deliveryCaller returns the session that issued a send_to, from evidence that
// identifies the caller rather than a value picked on its behalf: a name it
// asserted, or an identity the MCP layer attributed through the environment, a
// PID or a working directory. "" means the caller is the daemon itself — its
// internal reports carry no caller at all — or that no source matched.
//
// The broad resolveSessionID fallback must NOT be used here. It is
// last-writer-wins across every session on the machine, so on a guessed
// identity the self-wake guard would either suppress a delivery that was meant
// to happen or wake the very session that guard exists to protect. The cost of
// that choice is stated on sendViaOpencodeAPI.
//
// Two of the sources are weaker than the rest — a caller can assert `sender`
// and can send `_caller_pid` itself — so this identifies a caller well enough
// to refuse waking it, and is not strong enough to authorize anything.
func (h *Handler) deliveryCaller(params map[string]any) string {
	for _, key := range []string{"sender", "session_id"} {
		if sid, ok := params[key].(string); ok && sid != "" {
			return sid
		}
	}
	return h.provenSessionID(params, "_sender")
}

// channelPromptText renders a channel message the way the receiving agent would
// have seen it injected: sender and message type in the text, because the
// metadata of an API-delivered turn is not rendered by every client.
//
// It mirrors the header and the directive the proxy puts around channel
// messages (internal/proxy/think.go), with one deliberate difference: the
// directive names the reply target as a placeholder instead of embedding the
// address. The proxy can embed it — it renders a message the session itself
// pulled in — but the label here is caller-supplied, and a caller-supplied
// string must not become part of an instruction that another agent executes.
func channelPromptText(sender, msgType, content string) string {
	// The proxy renders the body the same way; channeltext keeps the two
	// transports in step and keeps the body from adding a header line.
	return fmt.Sprintf("📨 Von %s [%s]:\n%s%s", channeltext.SenderLabel(sender), channeltext.MessageType(msgType), channeltext.Body(content), channelPromptDirective(msgType))
}

// channelPromptDirective states the reply rule that belongs to a channel
// message, as the proxy states it for injected messages wherever the wording is
// transport-neutral.
//
// Leaving it out is not free: a daemon report is a status message, and without
// the rule a woken session may answer it — which turns a notification into a
// conversation and makes every periodic status ping spend a turn.
func channelPromptDirective(msgType string) string {
	switch msgType {
	case "ack", "status":
		return "\n\nDIRECTIVE: Message is an acknowledgement/status update. Do NOT reply — no action needed."
	case "response":
		return "\n\nDIRECTIVE: Result/response received." +
			"\n1. Process the response." +
			"\n2. Only reply if open questions remain: send_to(target='<the sender of the message above>', content='...', msg_type='command')" +
			"\n3. If no open questions: continue working, do NOT reply with ack."
	default: // "command", and anything the caller invented
		return "\n\nDIREKTIVE: Verarbeite die Nachricht oben." +
			"\n1. Arbeite die Aufgabe ab." +
			"\n2. Zum Antworten: send_to(target='<Absender der Nachricht oben>', content='...', msg_type='response')"
	}
}

// terminalAgentStatuses are the agent states in which a session must not be
// woken: the daemon stopped the agent, or its process exited. A push would spend
// a turn outside the budget that stop was meant to enforce, which is also why
// the heartbeat refuses to ping these agents. The names are the ones the daemon
// actually writes (handler_agents.go, heartbeat.go).
var terminalAgentStatuses = map[string]bool{
	"stopped": true, "finished": true, "failed": true, "crashed": true, "error": true,
}

// agentOutOfService reports whether an address belongs to an agent the daemon
// has taken out of service.
func (h *Handler) agentOutOfService(address string) bool {
	agent, err := h.store.AgentGetAnyBySession(address)
	if err != nil || agent == nil {
		return false
	}
	return terminalAgentStatuses[agent.Status]
}

// sendViaOpencodeAPI pushes one channel message into an opencode session's
// inbox through its own service API, so a session that is not making a request
// right now still reacts to it. Without this, a stored message only ever
// arrives when the target happens to ask the proxy something.
//
// The result is deliveryStored whenever the message has to keep its stored path
// — the target is not an opencode session, no service is running (a v1-only
// install), the caller is the target itself, the agent behind the address is out
// of service, or the service refused. Those are the cases in which the stored
// path still carries the message.
//
// A failure that proves nothing — a deadline, a transport error, a 5xx, a status
// that is neither an answer nor a refusal — is the one case that does not: the
// message stays claimed and the caller is told the outcome is unknown. It is
// then neither delivered a second time nor handed back to the stored path, so a
// service that never took the turn loses that message. That is a deliberate
// price, argued at the branch below.
//
// The caller's own session is never woken, as far as the caller's identity can
// be established: a caller that asserts an identity other than its own is
// believed, and an unrecognised caller that addresses its own session is not
// caught. The cost of that is one extra turn. The alternative — judging the
// caller by the last session to touch the proxy — silently suppressed the
// daemon reports this delivery exists for, which is worse and harder to notice.
func (h *Handler) sendViaOpencodeAPI(d channelDelivery) (deliveryOutcome, error) {
	if !isOpencodeAddress(d.Address) {
		return deliveryStored, nil
	}
	if d.Caller != "" && containsString(h.store.ChannelAddresses(d.Caller), d.Address) {
		return deliveryStored, nil
	}
	if h.agentOutOfService(d.Address) {
		return deliveryStored, nil
	}

	// Claim the row before the push. The other two consumers — the proxy
	// injection and the heartbeat ping — both select on delivered = 0 and
	// neither re-checks after reading, so a reader that starts after this claim
	// finds nothing and cannot carry a second copy. A reader that already had
	// the row in hand when the claim happened is not ordered against it; what is
	// left of that window is the distance from the insert to this statement —
	// the two lookups above, not the service probe, which comes later. Only a
	// reader that claims as well could close it, and that is the proxy's side.
	claimed, err := h.store.ClaimChannelMessage(d.MessageID)
	if err != nil {
		return deliveryStored, err
	}
	if !claimed {
		log.Printf("[send_to] message %d was already taken by another consumer, not pushing", d.MessageID)
		return deliveryStored, nil
	}

	client := opencodeV2Service()
	if client == nil {
		h.releaseClaim(d.MessageID)
		return deliveryStored, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendDeliveryTimeout)
	defer cancel()
	_, err = client.PromptUserTurn(ctx, strings.TrimPrefix(d.Address, opencodeAddressPrefix),
		channelPromptText(d.Sender, d.MsgType, d.Content), opencode.PromptOptions{
			Delivery: opencode.DeliveryQueue,
			Resume:   true,
			Metadata: map[string]any{
				"sender":     channeltext.SenderLabel(d.Sender),
				"message_id": d.MessageID,
				"msg_type":   channeltext.MessageType(d.MsgType),
			},
		})
	if err != nil {
		if opencode.Rejected(err) {
			// The service decided not to take the turn, so nothing is pending:
			// hand the message back to the stored path.
			h.releaseClaim(d.MessageID)
			return deliveryStored, err
		}
		// Unconfirmed: the request may have been taken before the client gave
		// up, or the service may have failed on it after taking it. Neither
		// outcome is proven, and the two ways out lead opposite ways.
		//
		// Releasing it lets the stored path carry a second copy of a turn the
		// target may already have; keeping the claim means a service that never
		// took the turn leaves the message claimed and undelivered. This keeps
		// the claim, because the failure it risks is a target doing the work
		// twice, while the failure it accepts is one report that never arrives —
		// and the caller is told the outcome is unknown rather than given a
		// verdict. A claim that is never released is invisible to every
		// consumer, so it has to be found in this log line.
		log.Printf("[send_to] message %d: delivery to %q is unconfirmed (%v) — claim kept, the target may already have the turn", d.MessageID, d.Address, err)
		return deliveryUnconfirmed, err
	}
	return deliveryAPI, nil
}

// releaseClaim hands a claimed message back to the stored path. A claim that
// cannot be released leaves the message invisible to every consumer, so the one
// retry is not politeness: without it the message is lost.
func (h *Handler) releaseClaim(messageID int64) {
	if err := h.store.ReleaseChannelMessage(messageID); err == nil {
		return
	}
	if err := h.store.ReleaseChannelMessage(messageID); err != nil {
		log.Printf("[send_to] message %d stays claimed after an unsent delivery and cannot be released — it will not arrive: %v", messageID, err)
	}
}
